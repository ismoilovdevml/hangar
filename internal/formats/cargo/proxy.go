package cargo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

const (
	maxIndexBytes  = 32 << 20 // one remote index file (crates with thousands of versions)
	maxConfigBytes = 64 << 10 // the remote's config.json
)

func (h *handler) metadataTTL(repo *core.Repository) time.Duration {
	if d := repo.Config.Proxy.MetadataTTL.D(); d > 0 {
		return d
	}
	return DefaultMetadataTTL
}

// remoteBase is the remote index URL with a trailing slash.
func remoteBase(repo *core.Repository) string {
	return strings.TrimSuffix(repo.Config.Proxy.RemoteURL, "/") + "/"
}

// proxyIndex returns the remote's index file of name, minus the versions
// quarantined in this repository.
func (h *handler) proxyIndex(ctx context.Context, repo *core.Repository, name string) (*indexFile, error) {
	doc, err := h.remoteDoc(ctx, repo, IndexPath(name), indexCachePath(name), maxIndexBytes)
	if err != nil {
		return nil, err
	}
	blocked, err := h.quarantinedVersions(ctx, repo, name)
	if err != nil {
		return nil, err
	}
	out := &indexFile{modified: doc.modified}
	for _, l := range splitLines(doc.body) {
		if !blocked[l.vers] {
			out.lines = append(out.lines, l)
		}
	}
	return out, nil
}

// quarantinedVersions returns the cached versions of name that are quarantined.
func (h *handler) quarantinedVersions(ctx context.Context, repo *core.Repository, name string) (map[string]bool, error) {
	comps, err := h.crateComponents(ctx, repo.ID, name)
	if err != nil {
		return nil, err
	}
	var out map[string]bool
	for _, c := range comps {
		if c.Quarantined {
			if out == nil {
				out = map[string]bool{}
			}
			out[c.Version] = true
		}
	}
	return out, nil
}

// remoteDocument is a remote index document as served from the cache.
type remoteDocument struct {
	body     []byte
	modified time.Time
}

// remoteDoc returns the remote document at remotePath (relative to the
// remote URL), cached at cachePath: from cache while fresh
// (ProxyConfig.MetadataTTL), revalidated with ETag / Last-Modified after
// that, and served stale while the remote fails.
func (h *handler) remoteDoc(ctx context.Context, repo *core.Repository, remotePath, cachePath string, limit int64) (*remoteDocument, error) {
	key := repo.ID + "/" + cachePath
	cached, asset := h.cachedDoc(ctx, repo, cachePath, limit)
	if cached != nil {
		fetched, _ := time.Parse(time.RFC3339Nano, attrString(asset.Attributes, "fetched_at"))
		h.mu.Lock()
		if t := h.checked[key]; t.After(fetched) {
			fetched = t
		}
		h.mu.Unlock()
		if time.Since(fetched) < h.metadataTTL(repo) {
			proxy.NoteCacheHit(ctx, asset.Size)
			return cached, nil
		}
	}
	v, err, _ := h.flight.Do("doc:"+key, func() (any, error) {
		doc, err := h.refreshDoc(context.WithoutCancel(ctx), repo, remotePath, cachePath, limit, cached, asset)
		return refreshed{doc: doc, fromCache: doc != nil && doc == cached}, err
	})
	res, _ := v.(refreshed)
	if err != nil || !res.fromCache {
		proxy.NoteCacheMiss(ctx)
	} else if asset != nil {
		proxy.NoteCacheHit(ctx, asset.Size)
	}
	if err != nil {
		return nil, err
	}
	return res.doc, nil
}

// refreshed is the outcome of one remote revalidation, shared by the
// requests collapsed into it.
type refreshed struct {
	doc       *remoteDocument
	fromCache bool
}

func (h *handler) cachedDoc(ctx context.Context, repo *core.Repository, cachePath string, limit int64) (*remoteDocument, *core.Asset) {
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, cachePath)
	if err != nil {
		return nil, nil
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, nil
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		h.log.WarnContext(ctx, "cached index blob missing", "repo", repo.Name, "path", cachePath, "err", err)
		return nil, nil
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, limit))
	if err != nil {
		return nil, nil
	}
	doc := &remoteDocument{body: body}
	doc.modified, _ = time.Parse(time.RFC3339Nano, attrString(a.Attributes, "last_modified"))
	return doc, a
}

func (h *handler) refreshDoc(ctx context.Context, repo *core.Repository, remotePath, cachePath string, limit int64,
	cached *remoteDocument, asset *core.Asset) (*remoteDocument, error) {
	key := repo.ID + "/" + cachePath
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	req := core.UpstreamRequest{Path: remotePath}
	if asset != nil {
		req.Conditional.ETag = attrString(asset.Attributes, "etag")
		req.Conditional.LastModified, _ = time.Parse(time.RFC3339Nano, attrString(asset.Attributes, "last_modified"))
	}
	stale := func(cause error) (*remoteDocument, error) {
		if cached != nil && !errors.Is(cause, core.ErrNotFound) {
			h.log.WarnContext(ctx, "remote unavailable, serving cached index file", "repo", repo.Name, "path", remotePath, "err", cause)
			return cached, nil
		}
		return nil, cause
	}
	resp, err := up.Fetch(ctx, req)
	if err != nil {
		return stale(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.NotModified && cached != nil:
		h.mu.Lock()
		h.checked[key] = time.Now()
		h.mu.Unlock()
		return cached, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusUnavailableForLegalReasons:
		return nil, fmt.Errorf("%s not found on remote: %w", remotePath, core.ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return stale(fmt.Errorf("remote answered %d for %s: %w", resp.StatusCode, remotePath, core.ErrUpstream))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return stale(fmt.Errorf("read remote %s: %w: %w", remotePath, err, core.ErrUpstream))
	}
	if int64(len(body)) > limit {
		return stale(fmt.Errorf("remote %s exceeds %d bytes: %w", remotePath, limit, core.ErrUpstream))
	}
	doc := &remoteDocument{body: body, modified: resp.LastModified}
	if err := h.storeDoc(ctx, repo, cachePath, body, resp); err != nil {
		// Serving the fresh document matters more than caching it.
		h.log.WarnContext(ctx, "caching index file failed", "repo", repo.Name, "path", cachePath, "err", err)
	}
	h.mu.Lock()
	h.checked[key] = time.Now()
	h.mu.Unlock()
	return doc, nil
}

func (h *handler) storeDoc(ctx context.Context, repo *core.Repository, cachePath string, body []byte, resp *core.UpstreamResponse) error {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return err
	}
	d, n, err := bs.Put(ctx, bytes.NewReader(body))
	if err != nil {
		return err
	}
	attrs := map[string]any{"fetched_at": nowString(), "etag": resp.ETag}
	if !resp.LastModified.IsZero() {
		attrs["last_modified"] = resp.LastModified.UTC().Format(time.RFC3339Nano)
	}
	ct := "text/plain; charset=utf-8"
	if strings.HasSuffix(cachePath, ".json") {
		ct = "application/json"
	}
	_, _, err = h.deps.Components.Publish(ctx, repo, core.PublishRequest{Assets: []*core.Asset{{
		Path: cachePath, Kind: core.AssetMetadata, BlobDigest: d, Size: n, ContentType: ct, Attributes: attrs,
	}}})
	return err
}

// downloadTemplate is the remote's crate download template: the format
// config's download_url, else the "dl" of the remote's config.json, else
// the crates.io CDN.
func (h *handler) downloadTemplate(ctx context.Context, repo *core.Repository) string {
	if cfg := formatConfig(repo); cfg.DownloadURL != "" {
		return cfg.DownloadURL
	}
	doc, err := h.remoteDoc(ctx, repo, "config.json", remoteConfigPath, maxConfigBytes)
	if err == nil {
		var c struct {
			DL string `json:"dl"`
		}
		if json.Unmarshal(doc.body, &c) == nil && c.DL != "" {
			if u, err := url.Parse(c.DL); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
				return c.DL
			}
		}
	}
	h.log.WarnContext(ctx, "remote config.json unusable, downloading from the crates.io CDN", "repo", repo.Name, "err", err)
	return DefaultDownloadURL
}

// cacheCrate downloads a .crate listed in the remote's index, verifies it
// against the index cksum and stores it. Crates are immutable: once cached
// they are served without asking the remote.
func (h *handler) cacheCrate(ctx context.Context, repo *core.Repository, name, version string) (*core.Asset, error) {
	v, err, _ := h.flight.Do("crate:"+repo.ID+"/"+name+"/"+version, func() (any, error) {
		return h.fetchCrate(context.WithoutCancel(ctx), repo, name, version)
	})
	if err != nil {
		return nil, err
	}
	return v.(*core.Asset), nil
}

func (h *handler) fetchCrate(ctx context.Context, repo *core.Repository, name, version string) (*core.Asset, error) {
	if a, err := h.deps.Components.GetAsset(ctx, repo.ID, cratePath(name, version)); err == nil {
		return a, nil // a concurrent request cached it
	}
	f, err := h.proxyIndex(ctx, repo, name)
	if err != nil {
		return nil, err
	}
	var line []byte
	for _, l := range f.lines {
		if l.vers == version {
			line = l.raw
			break
		}
	}
	if line == nil {
		return nil, fmt.Errorf("crate `%s@%s` is not in the remote index: %w", name, version, core.ErrNotFound)
	}
	var entry map[string]any
	if err := json.Unmarshal(line, &entry); err != nil {
		return nil, fmt.Errorf("remote index line of %s@%s: %w", name, version, core.ErrUpstream)
	}
	display, _ := entry["name"].(string)
	cksum, _ := entry["cksum"].(string)
	if !ValidCrateName(display) || !strings.EqualFold(display, name) {
		return nil, fmt.Errorf("remote index line of %s@%s names crate %q: %w", name, version, display, core.ErrUpstream)
	}
	src := expandDownload(h.downloadTemplate(ctx, repo), display, version, cksum)
	body, err := h.fetchURL(ctx, repo, src)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	d, n, err := bs.Put(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("store %s-%s.crate: %w", name, version, err)
	}
	if cksum == "" {
		h.log.WarnContext(ctx, "remote index line without cksum; crate cached unverified", "repo", repo.Name, "crate", name, "version", version)
	} else if !strings.EqualFold(cksum, d.Hex()) {
		return nil, fmt.Errorf("remote crate %s-%s does not match the index cksum: %w", name, version, core.ErrUpstream)
	}
	yanked, _ := entry["yanked"].(bool)
	asset := &core.Asset{
		Path: cratePath(name, version), Kind: core.AssetContent, BlobDigest: d, Size: n, ContentType: crateContentType,
		Attributes: map[string]any{"filename": name + "-" + version + ".crate", "remote_url": src},
	}
	_, assets, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: name, Version: version, Attributes: map[string]any{
			"display_name": display, "index_entry": entry, "yanked": yanked, "prerelease": isPrerelease(version),
		}},
		Assets: []*core.Asset{asset},
	})
	if err != nil {
		return nil, err
	}
	return assets[0], nil
}

// fetchURL GETs a crate download URL through the repository's Upstream: a
// URL under the remote URL as a remote path, any other as a related-host URL
// (static.crates.io for index.crates.io). The download template's host is
// trusted as a related host because the administrator configured it or the
// remote declared it; the remote's credentials are never sent there.
func (h *handler) fetchURL(ctx context.Context, repo *core.Repository, raw string) (io.ReadCloser, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("crate download URL %q: %w", raw, core.ErrUpstream)
	}
	base := remoteBase(repo)
	req := core.UpstreamRequest{Path: strings.TrimPrefix(raw, base)}
	if !strings.HasPrefix(raw, base) {
		req = core.UpstreamRequest{URL: raw, RelatedHosts: []string{u.Host}}
	}
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	resp, err := up.Fetch(ctx, req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, nil
	case http.StatusNotFound, http.StatusGone, http.StatusForbidden:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("remote crate %s: %w", raw, core.ErrNotFound)
	default:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("remote answered %d for %s: %w", resp.StatusCode, raw, core.ErrUpstream)
	}
}

func attrString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}
