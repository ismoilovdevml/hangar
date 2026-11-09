package gomod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

func (h *handler) metadataTTL(repo *core.Repository) time.Duration {
	if d := repo.Config.Proxy.MetadataTTL.D(); d > 0 {
		return d
	}
	return DefaultMetadataTTL
}

// fetch requests a path relative to the proxy's remote URL.
func (h *handler) fetch(ctx context.Context, repo *core.Repository, path string, cond core.Conditional) (*core.UpstreamResponse, error) {
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	return up.Fetch(ctx, core.UpstreamRequest{Path: path, Conditional: cond})
}

// remoteNotFound reports the remote's 404 or 410 as ErrNotFound, keeping
// its explanation (the go command prints it).
func remoteNotFound(resp *core.UpstreamResponse, what string) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("%s: remote answered %d: %s: %w", what, resp.StatusCode, msg, core.ErrNotFound)
}

// proxyDoc returns the remote's version list or latest answer for mod,
// from the cache while fresh (ProxyConfig.MetadataTTL), revalidating with
// ETag/Last-Modified after that, and serving the stale copy when the remote
// fails or the repository is offline.
func (h *handler) proxyDoc(ctx context.Context, repo *core.Repository, mod string, k kind) ([]byte, error) {
	path := listPath(mod)
	if k == kindLatest {
		path = latestPath(mod)
	}
	key := repo.ID + "/" + path
	cached, cachedAsset := h.cachedDoc(ctx, repo, path)
	if cached != nil {
		fetched, _ := time.Parse(time.RFC3339Nano, attrString(cachedAsset.Attributes, "fetched_at"))
		h.mu.Lock()
		if t := h.checked[key]; t.After(fetched) {
			fetched = t
		}
		h.mu.Unlock()
		if time.Since(fetched) < h.metadataTTL(repo) {
			proxy.NoteCacheHit(ctx, cachedAsset.Size)
			return cached, nil
		}
	}
	type result struct {
		b         []byte
		fromCache bool
	}
	v, err, _ := h.flight.Do("doc:"+key, func() (any, error) {
		b, fromCache, err := h.refreshDoc(context.WithoutCancel(ctx), repo, mod, k, path, cached, cachedAsset)
		return result{b, fromCache}, err
	})
	res, _ := v.(result)
	if err != nil || !res.fromCache {
		proxy.NoteCacheMiss(ctx)
	} else if cachedAsset != nil {
		proxy.NoteCacheHit(ctx, cachedAsset.Size)
	}
	if err != nil {
		return nil, err
	}
	return res.b, nil
}

func (h *handler) cachedDoc(ctx context.Context, repo *core.Repository, path string) ([]byte, *core.Asset) {
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, path)
	if err != nil {
		return nil, nil
	}
	b, err := h.readAsset(ctx, repo, a, maxListBytes)
	if err != nil {
		h.log.WarnContext(ctx, "cached go document unreadable", "repo", repo.Name, "path", path, "err", err)
		return nil, nil
	}
	return b, a
}

func (h *handler) refreshDoc(ctx context.Context, repo *core.Repository, mod string, k kind, path string, cached []byte, cachedAsset *core.Asset) ([]byte, bool, error) {
	key := repo.ID + "/" + path
	stale := func(cause error) ([]byte, bool, error) {
		if cached != nil {
			h.log.WarnContext(ctx, "remote unavailable, serving cached document", "repo", repo.Name, "path", path, "err", cause)
			return cached, true, nil
		}
		return nil, false, cause
	}
	remotePath, err := escaped(mod, "", k)
	if err != nil {
		return nil, false, err
	}
	var cond core.Conditional
	if cachedAsset != nil {
		cond.ETag = attrString(cachedAsset.Attributes, "etag")
		cond.LastModified, _ = time.Parse(time.RFC3339Nano, attrString(cachedAsset.Attributes, "last_modified"))
	}
	resp, err := h.fetch(ctx, repo, remotePath, cond)
	if err != nil {
		return stale(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.NotModified && cached != nil:
		h.mu.Lock()
		h.checked[key] = time.Now()
		h.mu.Unlock()
		return cached, true, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, false, remoteNotFound(resp, mod)
	case resp.StatusCode != http.StatusOK:
		return stale(fmt.Errorf("remote answered %d for %s: %w", resp.StatusCode, remotePath, core.ErrUpstream))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes+1))
	if err != nil {
		return stale(fmt.Errorf("read %s: %w: %w", remotePath, err, core.ErrUpstream))
	}
	if len(b) > maxListBytes {
		return stale(fmt.Errorf("remote %s exceeds %d bytes: %w", remotePath, maxListBytes, core.ErrUpstream))
	}
	if k == kindLatest {
		var doc infoDoc
		if err := json.Unmarshal(b, &doc); err != nil || !canonical(doc.Version) {
			return stale(fmt.Errorf("remote %s is not a version info document: %w", remotePath, core.ErrUpstream))
		}
	}
	if err := h.storeDoc(ctx, repo, mod, path, b, resp); err != nil {
		// Serving the fresh answer matters more than caching it.
		h.log.WarnContext(ctx, "caching go document failed", "repo", repo.Name, "path", path, "err", err)
	}
	h.mu.Lock()
	h.checked[key] = time.Now()
	h.mu.Unlock()
	return b, false, nil
}

func (h *handler) storeDoc(ctx context.Context, repo *core.Repository, mod, path string, b []byte, resp *core.UpstreamResponse) error {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return err
	}
	d, n, err := bs.Put(ctx, bytes.NewReader(b))
	if err != nil {
		return err
	}
	attrs := map[string]any{"fetched_at": time.Now().UTC().Format(time.RFC3339Nano), "etag": resp.ETag, "module": mod}
	if !resp.LastModified.IsZero() {
		attrs["last_modified"] = resp.LastModified.UTC().Format(time.RFC3339Nano)
	}
	ct := "text/plain; charset=utf-8"
	if strings.HasSuffix(path, "/@latest") {
		ct = "application/json"
	}
	_, _, err = h.deps.Components.Publish(ctx, repo, core.PublishRequest{Assets: []*core.Asset{{
		Path: path, Kind: core.AssetMetadata, BlobDigest: d, Size: n, ContentType: ct, Attributes: attrs,
	}}})
	return err
}

// resolveQuery asks the remote for the .info of a query that is not a
// canonical version (a branch, tag or commit hash: "go get mod@main").
// The answer moves with the branch, so it is not cached.
func (h *handler) resolveQuery(ctx context.Context, repo *core.Repository, mod, query string) ([]byte, error) {
	p, err := escaped(mod, query, kindInfo)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", err.Error(), core.ErrNotFound)
	}
	proxy.NoteCacheMiss(ctx)
	resp, err := h.fetch(ctx, repo, p, core.Conditional{})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		return nil, remoteNotFound(resp, mod+"@"+query)
	default:
		return nil, fmt.Errorf("remote answered %d for %s: %w", resp.StatusCode, p, core.ErrUpstream)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxInfoBytes+1))
	if err != nil || len(b) > maxInfoBytes {
		return nil, fmt.Errorf("remote %s unreadable or too large: %w", p, core.ErrUpstream)
	}
	var doc infoDoc
	if json.Unmarshal(b, &doc) != nil || !canonical(doc.Version) {
		return nil, fmt.Errorf("remote %s is not a version info document: %w", p, core.ErrUpstream)
	}
	return b, nil
}

// proxyFile returns a cached .info, .mod or .zip, downloading it first on a
// miss. They are immutable: once cached they are served without asking the
// remote again.
func (h *handler) proxyFile(ctx context.Context, repo *core.Repository, mod, ver string, k kind) (*core.Asset, error) {
	path := filePath(mod, ver, k)
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, path)
	if err == nil {
		proxy.NoteCacheHit(ctx, a.Size)
		return a, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	proxy.NoteCacheMiss(ctx)
	v, err, _ := h.flight.Do("file:"+repo.ID+"/"+path, func() (any, error) {
		return h.fetchFile(context.WithoutCancel(ctx), repo, mod, ver, k)
	})
	if err != nil {
		return nil, err
	}
	return v.(*core.Asset), nil
}

func (h *handler) fetchFile(ctx context.Context, repo *core.Repository, mod, ver string, k kind) (*core.Asset, error) {
	path := filePath(mod, ver, k)
	if a, err := h.deps.Components.GetAsset(ctx, repo.ID, path); err == nil {
		return a, nil // a concurrent request cached it
	}
	remotePath, err := escaped(mod, ver, k)
	if err != nil {
		return nil, err
	}
	resp, err := h.fetch(ctx, repo, remotePath, core.Conditional{})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		return nil, remoteNotFound(resp, mod+"@"+ver+k.ext())
	default:
		return nil, fmt.Errorf("remote answered %d for %s: %w", resp.StatusCode, remotePath, core.ErrUpstream)
	}
	limit := int64(maxZipBytes)
	switch k {
	case kindInfo:
		limit = maxInfoBytes
	case kindMod:
		limit = maxGoModBytes
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	var (
		d     core.Digest
		n     int64
		attrs = map[string]any{attrComplete: true}
	)
	switch k {
	case kindZip:
		d, n, err = bs.Put(ctx, io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, fmt.Errorf("store %s: %w", path, err)
		}
		if n > limit {
			return nil, fmt.Errorf("remote %s exceeds %d bytes: %w", remotePath, limit, core.ErrUpstream)
		}
		facts, err := h.checkStoredZip(ctx, bs, mod, ver, d, n)
		if err != nil {
			// Not wrapped: the remote's bad zip is a gateway error, not the client's.
			return nil, fmt.Errorf("remote zip of %s@%s: %s: %w", mod, ver, err.Error(), core.ErrUpstream)
		}
		attrs[attrGoMod] = facts.goMod != nil
		if facts.goVersion != "" {
			attrs[attrGo] = facts.goVersion
		}
	default:
		b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w: %w", remotePath, err, core.ErrUpstream)
		}
		if int64(len(b)) > limit {
			return nil, fmt.Errorf("remote %s exceeds %d bytes: %w", remotePath, limit, core.ErrUpstream)
		}
		if k == kindInfo {
			var doc infoDoc
			if json.Unmarshal(b, &doc) != nil || doc.Version != ver {
				return nil, fmt.Errorf("remote %s is not the info of %s: %w", remotePath, ver, core.ErrUpstream)
			}
			if !doc.Time.IsZero() {
				attrs[attrTime] = doc.Time.UTC().Format(time.RFC3339)
			}
		}
		if d, n, err = bs.Put(ctx, bytes.NewReader(b)); err != nil {
			return nil, fmt.Errorf("store %s: %w", path, err)
		}
	}
	_, assets, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: mod, Version: ver, Attributes: attrs},
		Assets: []*core.Asset{{Path: path, Kind: core.AssetContent, BlobDigest: d, Size: n, ContentType: k.contentType(),
			Attributes: map[string]any{"remote_path": remotePath}}},
	})
	if err != nil {
		return nil, err
	}
	return assets[0], nil
}

func attrString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}
