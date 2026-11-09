package helm

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// upstreamIndexPath is where a proxy keeps the remote's index.yaml as fetched.
const upstreamIndexPath = "_upstream/index.yaml"

// maxUpstreamIndex bounds a remote index.yaml read into memory; large
// public repositories publish indexes of tens of megabytes.
const maxUpstreamIndex = 128 << 20

// Cached index asset attributes.
const (
	attrFetchedAt    = "fetched_at"
	attrETag         = "etag"
	attrLastModified = "last_modified"
)

// parsedIndex is a remote index.yaml with every entry's URL rewritten to
// this repository's layout; origin keeps the absolute upstream URL.
type parsedIndex struct {
	digest  core.Digest
	entries []*entry
}

// remoteIndex returns the remote's directory URL (with a trailing slash,
// the base relative chart URLs resolve against) and its index.yaml URL. A
// remote URL ending in .yaml/.yml is the index itself.
func remoteIndex(repo *core.Repository) (dir, index string) {
	raw := repo.Config.Proxy.RemoteURL
	if l := strings.ToLower(raw); strings.HasSuffix(l, ".yaml") || strings.HasSuffix(l, ".yml") {
		return raw[:strings.LastIndex(raw, "/")+1], raw
	}
	dir = strings.TrimSuffix(raw, "/") + "/"
	return dir, dir + "index.yaml"
}

func attrString(a *core.Asset, k string) string {
	s, _ := a.Attributes[k].(string)
	return s
}

// fresh reports whether the cached remote index may be served without
// asking the remote (ProxyConfig.MetadataTTL; a 304 also refreshes it).
func (h *handler) fresh(repo *core.Repository, a *core.Asset) bool {
	at, _ := time.Parse(time.RFC3339Nano, attrString(a, attrFetchedAt))
	h.mu.Lock()
	if t := h.checked[repo.ID]; t.After(at) {
		at = t
	}
	h.mu.Unlock()
	return proxy.FreshnessOf(repo.Config.Proxy).IsFreshAt(core.AssetMetadata, at, h.now())
}

// upstreamIndex returns the cached remote index.yaml asset, refreshing it
// first when it is older than the metadata TTL. While the remote fails a
// stale copy is served.
func (h *handler) upstreamIndex(ctx context.Context, repo *core.Repository) (*core.Asset, error) {
	cached, err := h.deps.Components.GetAsset(ctx, repo.ID, upstreamIndexPath)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	if cached != nil && h.fresh(repo, cached) {
		proxy.NoteCacheHit(ctx, cached.Size)
		return cached, nil
	}
	res, err, _ := h.flight.Do("index:"+repo.ID, func() (any, error) {
		return h.refreshIndex(context.WithoutCancel(ctx), repo, cached)
	})
	if err != nil {
		proxy.NoteCacheMiss(ctx)
		return nil, err
	}
	a := res.(*core.Asset)
	if cached != nil && a.BlobDigest == cached.BlobDigest {
		proxy.NoteCacheHit(ctx, a.Size)
	} else {
		proxy.NoteCacheMiss(ctx)
	}
	return a, nil
}

func (h *handler) refreshIndex(ctx context.Context, repo *core.Repository, cached *core.Asset) (*core.Asset, error) {
	stale := func(cause error) (*core.Asset, error) {
		if cached != nil {
			h.log.WarnContext(ctx, "remote unavailable, serving cached index.yaml", "repo", repo.Name, "err", cause)
			return cached, nil
		}
		return nil, cause
	}
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	_, indexURL := remoteIndex(repo)
	req := core.UpstreamRequest{URL: indexURL, Index: true, Header: http.Header{"Accept": {"application/x-yaml, application/yaml, text/yaml, */*"}}}
	if cached != nil {
		req.Conditional.ETag = attrString(cached, attrETag)
		req.Conditional.LastModified, _ = time.Parse(time.RFC3339Nano, attrString(cached, attrLastModified))
	}
	resp, err := up.Fetch(ctx, req)
	if err != nil {
		return stale(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.NotModified && cached != nil:
		h.mu.Lock()
		h.checked[repo.ID] = h.now()
		h.mu.Unlock()
		return cached, nil
	case resp.StatusCode == http.StatusNotFound:
		return stale(fmt.Errorf("remote has no index.yaml at %s: %w", core.RedactURL(indexURL), core.ErrUpstream))
	case resp.StatusCode != http.StatusOK:
		return stale(fmt.Errorf("remote answered %d for index.yaml: %w", resp.StatusCode, core.ErrUpstream))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamIndex+1))
	if err != nil {
		return stale(fmt.Errorf("read remote index.yaml: %w: %w", err, core.ErrUpstream))
	}
	if len(body) > maxUpstreamIndex {
		return stale(fmt.Errorf("remote index.yaml exceeds %d bytes: %w", maxUpstreamIndex, core.ErrUpstream))
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	d, n, err := bs.Put(ctx, bytes.NewReader(body))
	if err != nil {
		return stale(fmt.Errorf("store remote index.yaml: %w", err))
	}
	idx, err := h.parseUpstream(ctx, repo, body, d)
	if err != nil {
		return stale(err)
	}
	h.mu.Lock()
	h.parsed[repo.ID] = idx
	h.checked[repo.ID] = h.now()
	h.mu.Unlock()
	attrs := map[string]any{attrFetchedAt: h.now().UTC().Format(time.RFC3339Nano)}
	if resp.ETag != "" {
		attrs[attrETag] = resp.ETag
	}
	if !resp.LastModified.IsZero() {
		attrs[attrLastModified] = resp.LastModified.UTC().Format(time.RFC3339Nano)
	}
	asset := &core.Asset{Path: upstreamIndexPath, Kind: core.AssetMetadata, BlobDigest: d, Size: n,
		ContentType: contentTypeYAML, Attributes: attrs, UpdatedAt: h.now()}
	_, stored, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{Assets: []*core.Asset{asset}})
	if err != nil {
		// Serving the fresh index matters more than caching it.
		h.log.WarnContext(ctx, "caching remote index.yaml failed", "repo", repo.Name, "err", err)
		return asset, nil
	}
	return stored[0], nil
}

// parseUpstream reads a remote index.yaml. Entries without a valid name,
// version or URL are left out, as Helm skips them too, and so are entries
// that do not decode (a malformed date, a string where a list belongs): one
// bad entry must not take every other chart of the remote down with it.
// Skipped entries are logged once per parse and counted in
// hangar_proxy_index_entries_skipped_total.
func (h *handler) parseUpstream(ctx context.Context, repo *core.Repository, body []byte, d core.Digest) (*parsedIndex, error) {
	var doc struct {
		APIVersion string               `yaml:"apiVersion"`
		Entries    map[string]yaml.Node `yaml:"entries"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("remote index.yaml is not valid YAML: %w: %w", err, core.ErrUpstream)
	}
	if doc.APIVersion == "" {
		return nil, fmt.Errorf("remote index.yaml has no apiVersion: %w", core.ErrUpstream)
	}
	dir, _ := remoteIndex(repo)
	base, err := url.Parse(dir)
	if err != nil {
		return nil, fmt.Errorf("remote URL: %w: %w", err, core.ErrInvalid)
	}
	idx := &parsedIndex{digest: d}
	skipped, example := 0, ""
	skip := func(key string, line int, why string) {
		skipped++
		if example == "" {
			example = fmt.Sprintf("entries.%s (line %d): %s", key, line, why)
		}
	}
	for key, list := range doc.Entries {
		if list.Kind != yaml.SequenceNode {
			if list.Kind != 0 && (list.Kind != yaml.ScalarNode || list.Tag != "!!null") {
				skip(key, list.Line, "not a list of chart versions")
			}
			continue
		}
		for _, node := range list.Content {
			var e *entry
			if err := node.Decode(&e); err != nil {
				skip(key, node.Line, err.Error())
				continue
			}
			if e == nil {
				continue
			}
			if len(e.URLs) == 0 {
				skip(key, node.Line, "no urls")
				continue
			}
			if e.Name == "" {
				e.Name = key
			}
			if e.APIVersion == "" {
				e.APIVersion = APIVersionV1
			}
			v, err := ParseVersion(e.Version)
			if err != nil || !ValidName(e.Name) {
				skip(key, node.Line, fmt.Sprintf("invalid name %q or version %q", e.Name, e.Version))
				continue
			}
			u, err := base.Parse(e.URLs[0])
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				skip(key, node.Line, "invalid url")
				continue
			}
			u.Fragment = ""
			e.ver, e.origin = v, u.String()
			e.URLs = []string{chartPath(e.Name, e.Version)}
			if dg := strings.ToLower(strings.TrimPrefix(e.Digest, "sha256:")); len(dg) == 64 && isHex(dg) {
				e.Digest = dg
			} else {
				e.Digest = ""
			}
			idx.entries = append(idx.entries, e)
		}
	}
	if skipped > 0 {
		proxy.SkippedIndexEntries(Name, repo.Name, skipped)
		h.log.WarnContext(ctx, "helm proxy left malformed entries of the remote index.yaml out", "repo", repo.Name,
			"skipped", skipped, "kept", len(idx.entries), "first", example)
	}
	return idx, nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// parsedFor returns the parsed form of the cached index asset a.
func (h *handler) parsedFor(ctx context.Context, repo *core.Repository, a *core.Asset) (*parsedIndex, error) {
	h.mu.Lock()
	p := h.parsed[repo.ID]
	h.mu.Unlock()
	if p != nil && p.digest == a.BlobDigest {
		return p, nil
	}
	res, err, _ := h.flight.Do("parse:"+repo.ID+"/"+string(a.BlobDigest), func() (any, error) {
		bs, err := h.deps.Blobs.ForRepository(ctx, repo)
		if err != nil {
			return nil, err
		}
		rc, err := bs.Get(ctx, a.BlobDigest)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		body, err := io.ReadAll(io.LimitReader(rc, maxUpstreamIndex+1))
		if err != nil {
			return nil, err
		}
		p, err := h.parseUpstream(ctx, repo, body, a.BlobDigest)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.parsed[repo.ID] = p
		h.mu.Unlock()
		return p, nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*parsedIndex), nil
}

// proxyView is the remote's index minus the cached versions that are
// quarantined here and the charts the repository's routing rules exclude.
func (h *handler) proxyView(ctx context.Context, repo *core.Repository) (*view, error) {
	a, err := h.upstreamIndex(ctx, repo)
	if err != nil {
		return nil, err
	}
	key := rulesKey(repo) + string(a.BlobDigest)
	if v, ok := h.views.get(repo, key); ok {
		return v, nil
	}
	gen := h.views.generation(repo.Name)
	res, err, _ := h.flight.Do("pview:"+repo.ID+"/"+key, func() (any, error) {
		ctx := context.WithoutCancel(ctx)
		p, err := h.parsedFor(ctx, repo, a)
		if err != nil {
			return nil, err
		}
		comps, err := h.allComponents(ctx, repo, "")
		if err != nil {
			return nil, err
		}
		blocked := map[[2]string]bool{}
		for _, c := range comps {
			if c.Quarantined {
				blocked[[2]string{c.Name, c.Version}] = true
			}
		}
		es := p.entries
		var ex exclusions
		if len(blocked) > 0 || !repo.Config.Routing.Empty() {
			es = make([]*entry, 0, len(p.entries))
			for _, e := range p.entries {
				if b := repo.RoutingBlock(e.Name); b != nil {
					ex.add(b)
					continue
				}
				if !blocked[[2]string{e.Name, e.Version}] {
					es = append(es, e)
				}
			}
		}
		v := newView(es, h.now())
		v.blocks = ex.blocks
		h.views.put(gen, repo, key, v)
		return v, nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*view), nil
}

// proxyAsset returns the cached archive (or .prov) of name/version,
// fetching it from the URL the remote's index gives on a miss. Archives are
// immutable: once cached they are served without asking the remote.
func (h *handler) proxyAsset(ctx context.Context, repo *core.Repository, name, version string, prov bool) (*core.Asset, error) {
	path := chartPath(name, version)
	if prov {
		path = provPath(name, version)
	}
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, path)
	if err == nil {
		proxy.NoteCacheHit(ctx, a.Size)
		return a, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	v, err := h.proxyView(ctx, repo)
	if err != nil {
		return nil, err
	}
	e := v.find(name, version)
	if e == nil {
		return nil, fmt.Errorf("chart %s %s is not in the remote index: %w", name, version, core.ErrNotFound)
	}
	proxy.NoteCacheMiss(ctx)
	res, err, _ := h.flight.Do("file:"+repo.ID+"/"+path, func() (any, error) {
		return h.fetchFile(context.WithoutCancel(ctx), repo, e, path, prov)
	})
	if err != nil {
		return nil, err
	}
	return res.(*core.Asset), nil
}

func (h *handler) fetchFile(ctx context.Context, repo *core.Repository, e *entry, path string, prov bool) (*core.Asset, error) {
	if a, err := h.deps.Components.GetAsset(ctx, repo.ID, path); err == nil {
		return a, nil // a concurrent request cached it
	}
	src := e.origin
	if prov {
		src += provSuffix
	}
	body, err := h.fetchURL(ctx, repo, src, e.Name)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	limit := int64(maxChartSize)
	if prov {
		limit = maxProv
	}
	d, n, err := bs.Put(ctx, io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	if n > limit {
		return nil, fmt.Errorf("remote file %s exceeds %d bytes: %w", core.RedactURL(src), limit, core.ErrUpstream)
	}
	comp := &core.Component{Name: e.Name, Version: e.Version}
	asset := &core.Asset{Path: path, Kind: core.AssetContent, BlobDigest: d, Size: n, ContentType: contentTypeProv,
		Attributes: map[string]any{"remote_url": core.RedactURL(src)}}
	if !prov {
		if e.Digest != "" && e.Digest != d.Hex() {
			return nil, fmt.Errorf("remote archive %s does not match the digest in index.yaml: %w", core.RedactURL(src), core.ErrUpstream)
		}
		md, err := h.readStoredChart(ctx, bs, d)
		if err != nil {
			return nil, fmt.Errorf("remote archive %s: %w: %w", core.RedactURL(src), err, core.ErrUpstream)
		}
		if md.Name != e.Name || md.Version != e.Version {
			return nil, fmt.Errorf("remote archive %s holds %s %s, not %s %s: %w",
				core.RedactURL(src), md.Name, md.Version, e.Name, e.Version, core.ErrUpstream)
		}
		comp.Attributes = chartAttributes(md, d.Hex(), e.Created)
		asset.ContentType = contentTypeChart
	}
	_, stored, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{Component: comp, Assets: []*core.Asset{asset}})
	if err != nil {
		if errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrUnauthenticated) {
			// The reader may not fill the cache: serve the file uncached.
			h.log.WarnContext(ctx, "helm chart served without caching", "repo", repo.Name, "path", path, "err", err)
			asset.UpdatedAt = h.now()
			return asset, nil
		}
		return nil, fmt.Errorf("cache %s: %w", path, err)
	}
	return stored[0], nil
}

// fetchURL GETs an absolute URL from the remote's index through the
// repository's Upstream, which applies the SSRF guard, breaker, request
// collapsing, outbound proxy and rate limits, and sends the remote's
// credentials only to the remote's own origin. URLs on other hosts must be
// on DefaultChartHosts or the repository's related_hosts. chart is the
// routing identity of the file.
func (h *handler) fetchURL(ctx context.Context, repo *core.Repository, raw, chart string) (io.ReadCloser, error) {
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	resp, err := up.Fetch(ctx, core.UpstreamRequest{URL: raw, RelatedHosts: DefaultChartHosts, Package: chart})
	if err != nil {
		if errors.Is(err, core.ErrInvalid) {
			host := raw
			if u, perr := url.Parse(raw); perr == nil {
				host = u.Host
			}
			return nil, fmt.Errorf("the remote index links to %s; add it to the repository's related_hosts to allow it: %w", host, core.ErrUpstream)
		}
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound, http.StatusGone:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("remote file %s: %w", core.RedactURL(raw), core.ErrNotFound)
	default:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("remote answered %d for %s: %w", resp.StatusCode, core.RedactURL(raw), core.ErrUpstream)
	}
}
