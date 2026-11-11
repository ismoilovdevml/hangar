package repokit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// Attributes recorded on cached remote files.
const (
	AttrCachedAt             = "cached_at" // RFC 3339; last confirmed with the remote
	AttrUpstreamETag         = "upstream_etag"
	AttrUpstreamLastModified = "upstream_last_modified"
	AttrRemoteURL            = "remote_url"
)

// fillTimeout bounds one remote download into the cache. Fills are detached
// from the first client's connection.
const fillTimeout = time.Hour

// Fetch describes one file a proxy repository serves from its remote.
type Fetch struct {
	// Path is the repository path; the remote path is the same, relative to
	// the remote URL.
	Path string
	// Package is the routing identity of a package file (core.RoutingName):
	// the proxy's routing rules are checked for it before the cache and the
	// remote. Empty marks repository metadata (Release, repomd.xml and the
	// indexes they list): those are signed by the remote and passed through
	// unchanged, so routing rules cannot filter them without breaking the
	// signature; a blocked package stays listed there but cannot be
	// downloaded.
	Package string
	// TTL is how long a cached copy is served before it is revalidated; zero
	// caches it forever (content and content-addressed metadata).
	TTL time.Duration
	// Valid, when set, reports whether a cached copy may still be served:
	// false refetches it without validators (it no longer matches the
	// metadata that lists it).
	Valid func(*core.Asset) bool
	Kind  core.AssetKind
	// ContentType of the stored file; empty takes the remote's.
	ContentType string
	// Attributes are added to the stored asset.
	Attributes map[string]any
	// Describe, for content files, returns the component the stored file
	// belongs to (nil for none) and extra asset attributes. It may read the
	// blob from bs.
	Describe func(ctx context.Context, bs core.BlobStore, a *core.Asset) (*core.Component, map[string]any)
}

// RemotePath escapes a repository path for the remote.
func RemotePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// ProxyGet returns repo's cached copy of f.Path, fetching or revalidating it
// from the remote when absent, expired or invalid. A stale copy is served
// while the remote is unreachable. Remote 404s fail with core.ErrNotFound
// (the core upstream remembers them for the negative cache TTL).
func (b *Base) ProxyGet(ctx context.Context, repo *core.Repository, f Fetch) (*core.Asset, error) {
	if repo.Config.Proxy == nil {
		return nil, fmt.Errorf("proxy repository %q has no proxy config: %w", repo.Name, core.ErrInvalid)
	}
	if f.Package != "" {
		if err := routing.Check(ctx, repo, f.Package); err != nil {
			return nil, err
		}
	}
	cached, err := b.Deps.Components.GetAsset(ctx, repo.ID, f.Path)
	switch {
	case err == nil:
		valid := f.Valid == nil || f.Valid(cached)
		if valid && b.fresh(cached, f.TTL) {
			proxy.NoteCacheHit(ctx, cached.Size)
			return cached, nil
		}
	case errors.Is(err, core.ErrNotFound):
		cached = nil
	default:
		return nil, err
	}
	v, err, _ := b.fill.Do(repo.ID+"\x00"+f.Path, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fillTimeout)
		defer cancel()
		return b.fetch(fctx, repo, f, cached)
	})
	if err != nil {
		proxy.NoteCacheMiss(ctx, err)
		return nil, err
	}
	res := v.(fillResult)
	if res.fromCache {
		proxy.NoteCacheHit(ctx, res.asset.Size)
	} else {
		proxy.NoteCacheMiss(ctx, nil)
	}
	return res.asset, nil
}

type fillResult struct {
	asset     *core.Asset
	fromCache bool
}

func (b *Base) fresh(a *core.Asset, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	at, ok := AttrTime(a, AttrCachedAt)
	if !ok {
		at = a.UpdatedAt
	}
	return b.Now().Sub(at) < ttl
}

func (b *Base) fetch(ctx context.Context, repo *core.Repository, f Fetch, cached *core.Asset) (fillResult, error) {
	stale := func(cause error) (fillResult, error) {
		if cached != nil {
			b.Log.WarnContext(ctx, "serving stale cached copy", "repository", repo.Name, "path", f.Path, "err", cause)
			return fillResult{asset: cached, fromCache: true}, nil
		}
		return fillResult{}, cause
	}
	// A cached copy that no longer matches its index is fetched again in full.
	if cached != nil && f.Valid != nil && !f.Valid(cached) {
		cached = nil
	}
	up, err := b.Deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return fillResult{}, err
	}
	req := core.UpstreamRequest{Method: http.MethodGet, Path: RemotePath(f.Path), Package: f.Package, Index: f.Package == ""}
	if cached != nil {
		req.Conditional.ETag = AttrString(cached.Attributes, AttrUpstreamETag)
		req.Conditional.LastModified, _ = AttrTime(cached, AttrUpstreamLastModified)
	}
	resp, err := up.Fetch(ctx, req)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) && cached == nil {
			return fillResult{}, err
		}
		if !errors.Is(err, core.ErrUpstream) && !errors.Is(err, core.ErrNotFound) {
			err = fmt.Errorf("%w: %w", err, core.ErrUpstream)
		}
		return stale(err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.NotModified && cached != nil:
		refreshed := *cached
		refreshed.Attributes = b.cacheAttributes(resp, cached, f)
		out, err := b.publish(ctx, repo, &refreshed, nil)
		if err != nil {
			return stale(err)
		}
		return fillResult{asset: out, fromCache: true}, nil
	case resp.StatusCode == http.StatusOK:
		bs, err := b.Deps.Blobs.ForRepository(ctx, repo)
		if err != nil {
			return fillResult{}, err
		}
		d, n, err := bs.Put(ctx, resp.Body)
		if err != nil {
			return stale(fmt.Errorf("download %s: %w: %w", f.Path, err, core.ErrUpstream))
		}
		if resp.ContentLength >= 0 && n != resp.ContentLength {
			return stale(fmt.Errorf("download %s: got %d of %d bytes: %w", f.Path, n, resp.ContentLength, core.ErrUpstream))
		}
		ct := f.ContentType
		if ct == "" {
			ct = resp.Header.Get("Content-Type")
		}
		kind := f.Kind
		if kind == "" {
			kind = core.AssetMetadata
		}
		a := &core.Asset{Path: f.Path, Kind: kind, BlobDigest: d, Size: n, ContentType: ct}
		a.Attributes = b.cacheAttributes(resp, nil, f)
		var comp *core.Component
		if f.Describe != nil {
			var extra map[string]any
			comp, extra = f.Describe(ctx, bs, a)
			for k, v := range extra {
				a.Attributes[k] = v
			}
		}
		out, err := b.publish(ctx, repo, a, comp)
		if err != nil {
			return stale(err)
		}
		return fillResult{asset: out}, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return stale(fmt.Errorf("%s: remote answered %d: %w", f.Path, resp.StatusCode, core.ErrNotFound))
	default:
		return stale(fmt.Errorf("%s: remote answered %d: %w", f.Path, resp.StatusCode, core.ErrUpstream))
	}
}

func (b *Base) cacheAttributes(resp *core.UpstreamResponse, prev *core.Asset, f Fetch) map[string]any {
	attrs := map[string]any{}
	if prev != nil {
		for k, v := range prev.Attributes {
			attrs[k] = v
		}
	}
	for k, v := range f.Attributes {
		attrs[k] = v
	}
	attrs[AttrCachedAt] = b.Now().UTC().Format(time.RFC3339Nano)
	if resp.ETag != "" {
		attrs[AttrUpstreamETag] = resp.ETag
	}
	if !resp.LastModified.IsZero() {
		attrs[AttrUpstreamLastModified] = resp.LastModified.UTC().Format(time.RFC3339Nano)
	}
	return attrs
}

// publish stores a cached asset; ComponentService authorizes a proxy cache
// fill as a read by the requesting principal.
func (b *Base) publish(ctx context.Context, repo *core.Repository, a *core.Asset, comp *core.Component) (*core.Asset, error) {
	_, out, err := b.Deps.Components.Publish(ctx, repo, core.PublishRequest{Component: comp, Assets: []*core.Asset{a}})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

// MetadataTTL is repo's proxy metadata TTL, def when unset.
func MetadataTTL(repo *core.Repository, def time.Duration) time.Duration {
	if repo.Config.Proxy != nil && repo.Config.Proxy.MetadataTTL.D() > 0 {
		return repo.Config.Proxy.MetadataTTL.D()
	}
	return def
}
