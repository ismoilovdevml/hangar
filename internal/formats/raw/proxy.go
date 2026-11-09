package raw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// Asset attributes recorded on cached proxy content.
const (
	attrCachedAt             = "cached_at"              // RFC 3339; when the content was last confirmed upstream
	attrUpstreamETag         = "upstream_etag"          // validator for revalidation
	attrUpstreamLastModified = "upstream_last_modified" // RFC 3339
)

// fillTimeout bounds one upstream download into the cache. The fill is not
// tied to the first client's connection, so a disconnect does not waste it.
const fillTimeout = time.Hour

// proxyAsset returns the cached asset for clean, fetching or revalidating it
// upstream when absent or older than the repository's content TTL. When the
// remote is unreachable a stale copy is served.
func (h *handler) proxyAsset(ctx context.Context, repo *core.Repository, clean string) (*core.Asset, error) {
	cfg := repo.Config.Proxy
	if cfg == nil {
		return nil, fmt.Errorf("proxy repository %q has no proxy config: %w", repo.Name, core.ErrInvalid)
	}
	cached, err := h.deps.Components.GetAsset(ctx, repo.ID, clean)
	switch {
	case err == nil:
		if h.fresh(cached, cfg.ContentTTL.D()) {
			proxy.NoteCacheHit(ctx, cached.Size)
			return cached, nil
		}
	case errors.Is(err, core.ErrNotFound):
		cached = nil
	default:
		return nil, err
	}
	key := repo.ID + "\x00" + clean
	if cached == nil && h.neg.has(key) {
		return nil, fmt.Errorf("%s (cached upstream miss): %w", clean, core.ErrNotFound)
	}
	v, err, _ := h.fill.Do(key, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fillTimeout)
		defer cancel()
		return h.fetch(fctx, repo, clean, cached)
	})
	if err != nil {
		proxy.NoteCacheMiss(ctx)
		if errors.Is(err, core.ErrNotFound) {
			h.neg.add(key, cfg.NegativeCacheTTL.D())
		}
		return nil, err
	}
	res := v.(fillResult)
	if res.fromCache {
		proxy.NoteCacheHit(ctx, res.asset.Size)
	} else {
		proxy.NoteCacheMiss(ctx)
	}
	return res.asset, nil
}

// fillResult is the outcome of one upstream fill, shared by the requests
// collapsed into it.
type fillResult struct {
	asset     *core.Asset
	fromCache bool // the cached copy was served (revalidated with 304, or stale)
}

func (h *handler) fresh(a *core.Asset, ttl time.Duration) bool {
	if ttl <= 0 {
		return true
	}
	at, ok := attrTime(a, attrCachedAt)
	if !ok {
		at = a.UpdatedAt
	}
	return h.now().Sub(at) < ttl
}

// fetch downloads clean from repo's remote into the cache. cached, if not
// nil, is revalidated with a conditional request and returned when the
// remote cannot be used.
func (h *handler) fetch(ctx context.Context, repo *core.Repository, clean string, cached *core.Asset) (fillResult, error) {
	stale := func(cause error) (fillResult, error) {
		if cached != nil {
			h.deps.Logger.Warn("raw: serving stale cached content", "repository", repo.Name, "path", clean, "err", cause)
			return fillResult{asset: cached, fromCache: true}, nil
		}
		return fillResult{}, cause
	}
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return fillResult{}, err
	}
	req := core.UpstreamRequest{Method: http.MethodGet, Path: escapePath(clean), Package: clean}
	if cached != nil {
		req.Conditional.ETag = attrString(cached, attrUpstreamETag)
		req.Conditional.LastModified, _ = attrTime(cached, attrUpstreamLastModified)
	}
	resp, err := up.Fetch(ctx, req)
	if err != nil {
		if !errors.Is(err, core.ErrUpstream) {
			err = fmt.Errorf("%w: %w", err, core.ErrUpstream)
		}
		return stale(err)
	}
	defer func() { _ = resp.Body.Close() }()

	// ComponentService authorizes a proxy cache fill as a read by the
	// requesting principal (carried in ctx).
	switch {
	case resp.NotModified && cached != nil:
		refreshed := *cached
		refreshed.Attributes = cacheAttributes(h.now(), resp, cached)
		out, err := h.publish(ctx, repo, &refreshed)
		if err != nil {
			return stale(err)
		}
		return fillResult{asset: out, fromCache: true}, nil
	case resp.StatusCode == http.StatusOK:
		a, err := h.store(ctx, repo, clean, resp.Body, contentType(resp.Header.Get("Content-Type"), clean), nil)
		if err != nil {
			return stale(fmt.Errorf("download %s: %w: %w", clean, err, core.ErrUpstream))
		}
		if resp.ContentLength >= 0 && a.Size != resp.ContentLength {
			return stale(fmt.Errorf("download %s: got %d of %d bytes: %w", clean, a.Size, resp.ContentLength, core.ErrUpstream))
		}
		a.Attributes = cacheAttributes(h.now(), resp, nil)
		out, err := h.publish(ctx, repo, a)
		return fillResult{asset: out}, err
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// Content already cached stays available when the remote drops it.
		return stale(fmt.Errorf("%s: remote answered %d: %w", clean, resp.StatusCode, core.ErrNotFound))
	default:
		return stale(fmt.Errorf("%s: remote answered %d: %w", clean, resp.StatusCode, core.ErrUpstream))
	}
}

// cacheAttributes records when content was confirmed upstream and the
// validators for the next revalidation (kept from prev when a 304 omits them).
func cacheAttributes(now time.Time, resp *core.UpstreamResponse, prev *core.Asset) map[string]any {
	attrs := map[string]any{attrCachedAt: now.UTC().Format(time.RFC3339Nano)}
	etag, lm := resp.ETag, resp.LastModified
	if prev != nil {
		if etag == "" {
			etag = attrString(prev, attrUpstreamETag)
		}
		if lm.IsZero() {
			lm, _ = attrTime(prev, attrUpstreamLastModified)
		}
	}
	if etag != "" {
		attrs[attrUpstreamETag] = etag
	}
	if !lm.IsZero() {
		attrs[attrUpstreamLastModified] = lm.UTC().Format(time.RFC3339Nano)
	}
	return attrs
}
