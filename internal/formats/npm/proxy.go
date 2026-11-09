package npm

import (
	"context"
	"crypto/sha1" //nolint:gosec // npm's legacy dist.shasum is SHA-1; verified, not trusted alone
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// Asset attributes of cached upstream packuments.
const (
	attrFetchedAt    = "fetched_at"
	attrETag         = "etag"
	attrLastModified = "last_modified"
)

func attrString(a *core.Asset, k string) string {
	s, _ := a.Attributes[k].(string)
	return s
}

// fresh applies the proxy cache policy (MetadataTTL: unset = default,
// negative = always revalidate) to a cached packument.
func (h *handler) fresh(repo *core.Repository, a *core.Asset) bool {
	at, err := time.Parse(time.RFC3339Nano, attrString(a, attrFetchedAt))
	return err == nil && proxy.FreshnessOf(repo.Config.Proxy).IsFreshAt(core.AssetMetadata, at, h.f.now())
}

func (h *handler) readProxyDoc(ctx context.Context, repo *core.Repository, bs core.BlobStore, a *core.Asset, name pkgName) (*packument, error) {
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	doc, err := parsePackument(rc)
	if err != nil {
		return nil, fmt.Errorf("cached packument %s: %w", name.Full, err)
	}
	if doc.Name == "" {
		doc.Name = name.Full
	}
	doc.relativizeTarballs(name, repo.Config.Proxy.RemoteURL)
	doc.normalizeTags()
	return doc, nil
}

// cachedProxyDoc returns the cached packument without contacting the remote.
func (h *handler) cachedProxyDoc(ctx context.Context, repo *core.Repository, bs core.BlobStore, name pkgName, abbreviated bool) (*packument, error) {
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, proxyDocPath(name, abbreviated))
	if err != nil {
		return nil, err
	}
	return h.readProxyDoc(ctx, repo, bs, a, name)
}

func (h *handler) storeProxyDoc(ctx context.Context, repo *core.Repository, path string, d core.Digest, size int64, etag string, lastMod time.Time) (*core.Asset, error) {
	attrs := map[string]any{attrFetchedAt: h.f.now().UTC().Format(time.RFC3339Nano)}
	if etag != "" {
		attrs[attrETag] = etag
	}
	if !lastMod.IsZero() {
		attrs[attrLastModified] = lastMod.UTC().Format(http.TimeFormat)
	}
	_, assets, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{Assets: []*core.Asset{{
		Path: path, Kind: core.AssetMetadata, BlobDigest: d, Size: size,
		ContentType: mediaJSON, Attributes: attrs,
	}}})
	if err != nil {
		return nil, err
	}
	return assets[0], nil
}

// proxyDoc returns the remote packument, served from cache while it is
// younger than MetadataTTL and revalidated (conditional GET) afterwards. If
// the remote is unreachable a stale cached copy is served.
func (h *handler) proxyDoc(ctx context.Context, repo *core.Repository, name pkgName, abbreviated bool) (*packument, error) {
	path := proxyDocPath(name, abbreviated)
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	cached, err := h.deps.Components.GetAsset(ctx, repo.ID, path)
	switch {
	case errors.Is(err, core.ErrNotFound):
		cached = nil
	case err != nil:
		return nil, err
	}
	if cached != nil && h.fresh(repo, cached) {
		proxy.NoteCacheHit(ctx, cached.Size)
		return h.readProxyDoc(ctx, repo, bs, cached, name)
	}
	stale := func(cause error) (*packument, error) {
		if cached == nil {
			proxy.NoteCacheMiss(ctx)
			return nil, cause
		}
		h.deps.Logger.WarnContext(ctx, "npm proxy serving stale packument", "repository", repo.Name,
			"package", name.Full, "error", cause)
		proxy.NoteCacheHit(ctx, cached.Size)
		return h.readProxyDoc(ctx, repo, bs, cached, name)
	}
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	accept := mediaJSON
	if abbreviated {
		accept = mediaAbbreviated + "; q=1.0, " + mediaJSON + "; q=0.8"
	}
	req := core.UpstreamRequest{Path: name.Escaped(), Header: http.Header{"Accept": {accept}}}
	if cached != nil {
		req.Conditional.ETag = attrString(cached, attrETag)
		req.Conditional.LastModified, _ = http.ParseTime(attrString(cached, attrLastModified))
	}
	resp, err := up.Fetch(ctx, req)
	if err != nil {
		return stale(err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.NotModified && cached != nil:
		etag, lm := resp.ETag, resp.LastModified
		if etag == "" {
			etag = attrString(cached, attrETag)
		}
		if lm.IsZero() {
			lm, _ = http.ParseTime(attrString(cached, attrLastModified))
		}
		if _, err := h.storeProxyDoc(ctx, repo, path, cached.BlobDigest, cached.Size, etag, lm); err != nil {
			h.deps.Logger.WarnContext(ctx, "npm proxy cache refresh failed", "repository", repo.Name, "error", err)
		}
		proxy.NoteCacheHit(ctx, cached.Size)
		return h.readProxyDoc(ctx, repo, bs, cached, name)
	case resp.StatusCode == http.StatusOK:
		proxy.NoteCacheMiss(ctx)
		d, n, err := bs.Put(ctx, io.LimitReader(resp.Body, maxDocumentSize))
		if err != nil {
			return stale(fmt.Errorf("store upstream packument: %w", err))
		}
		a, err := h.storeProxyDoc(ctx, repo, path, d, n, resp.ETag, resp.LastModified)
		if err != nil {
			return nil, err
		}
		doc, err := h.readProxyDoc(ctx, repo, bs, a, name)
		if errors.Is(err, core.ErrInvalid) {
			return nil, fmt.Errorf("upstream packument for %s is not valid JSON: %w", name.Full, core.ErrUpstream)
		}
		return doc, err
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		proxy.NoteCacheMiss(ctx)
		return nil, fmt.Errorf("package %s not found upstream: %w", name.Full, core.ErrNotFound)
	default:
		return stale(fmt.Errorf("upstream answered %d for %s: %w", resp.StatusCode, name.Full, core.ErrUpstream))
	}
}

// expectedDist finds the upstream's declared checksums for a tarball in a
// cached packument, without contacting the remote, and the absolute URL the
// packument gave when the tarball lives outside the remote URL.
func (h *handler) expectedDist(ctx context.Context, repo *core.Repository, bs core.BlobStore, name pkgName, rel string) (string, manifest, string) {
	for _, abbr := range []bool{true, false} {
		doc, err := h.cachedProxyDoc(ctx, repo, bs, name, abbr)
		if err != nil {
			continue
		}
		if v, m := doc.versionForTarball(rel); m != nil {
			return v, m, doc.origins[rel]
		}
	}
	return "", nil, ""
}

// tarballRequest fetches rel from the remote, or from the tarball CDN the
// packument named when that host is one of the repository's related hosts
// (registries that serve tarballs from another host). Without a related
// host the remote is asked for the tarball under its own URL, as before.
// An origin under the remote URL (a remote that names the tarball
// differently, see canonicalTarball) is requested under its own path.
func tarballRequest(repo *core.Repository, rel, origin string) core.UpstreamRequest {
	if origin != "" {
		remote := strings.TrimSuffix(repo.Config.Proxy.RemoteURL, "/") + "/"
		if s, ok := strings.CutPrefix(origin, remote); ok {
			if p, err := decodePath(s); err == nil && !strings.Contains(p, "?") {
				return core.UpstreamRequest{Path: p}
			}
		}
		if u, err := url.Parse(origin); err == nil && core.MatchHost(repo.Config.Proxy.RelatedHosts, u.Host) {
			return core.UpstreamRequest{URL: origin}
		}
	}
	return core.UpstreamRequest{Path: rel}
}

// tarballHashes collects the digests npm declares for tarballs.
type tarballHashes struct {
	sha1, sha512 hash.Hash
}

func newTarballHashes() *tarballHashes {
	return &tarballHashes{sha1: sha1.New(), sha512: sha512.New()} //nolint:gosec // see import
}

func (t *tarballHashes) writer() io.Writer { return io.MultiWriter(t.sha1, t.sha512) }

// proxyTarball serves a tarball from cache or fetches, verifies and caches it.
func (h *handler) proxyTarball(ctx context.Context, repo *core.Repository, rel string) (*core.Asset, core.BlobStore, error) {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	pkgPath, _, _ := strings.Cut(rel, "/-/")
	name, err := parseName(pkgPath)
	if err != nil {
		return nil, nil, err
	}
	// Tarballs cached before scoped paths were made conventional are kept
	// under the remote's scoped file name.
	for _, p := range []string{rel, scopedFileTarball(name, rel)} {
		if p == "" {
			continue
		}
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, p)
		if err == nil && a.Kind == core.AssetContent {
			if err := h.checkQuarantine(ctx, a); err != nil {
				return nil, nil, err
			}
			proxy.NoteCacheHit(ctx, a.Size)
			return a, bs, nil
		}
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			return nil, nil, err
		}
	}
	version, exp, origin := h.expectedDist(ctx, repo, bs, name, rel)
	if version == "" {
		version = versionFromFile(name, rel)
	}
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	proxy.NoteCacheMiss(ctx)
	resp, err := up.Fetch(ctx, tarballRequest(repo, rel, origin))
	if err != nil {
		return nil, nil, err
	}
	// Without a cached packument (a lockfile install) the remote's own name
	// for a scoped tarball is unknown: try the scoped file form too.
	if alt := scopedFileTarball(name, rel); origin == "" && alt != "" &&
		(resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
		_ = resp.Body.Close()
		if resp, err = up.Fetch(ctx, core.UpstreamRequest{Path: alt}); err != nil {
			return nil, nil, err
		}
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusGone:
		return nil, nil, fmt.Errorf("tarball %s not found upstream: %w", rel, core.ErrNotFound)
	default:
		return nil, nil, fmt.Errorf("upstream answered %d for %s: %w", resp.StatusCode, rel, core.ErrUpstream)
	}
	hs := newTarballHashes()
	d, n, err := bs.Put(ctx, io.TeeReader(resp.Body, hs.writer()))
	if err != nil {
		return nil, nil, fmt.Errorf("store upstream tarball: %w", err)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return nil, nil, fmt.Errorf("tarball %s truncated (%d of %d bytes): %w", rel, n, resp.ContentLength, core.ErrUpstream)
	}
	if exp != nil {
		if err := verifyDist(exp, hs, d); err != nil {
			return nil, nil, fmt.Errorf("upstream tarball %s: %v: %w", rel, err, core.ErrUpstream) //nolint:errorlint // a bad remote is a gateway error, not a client error
		}
	}
	var comp *core.Component
	if version != "" {
		comp = &core.Component{Namespace: name.Scope, Name: name.Base, Version: version}
		if exp != nil {
			comp.Attributes = componentAttributes(exp)
		}
	}
	_, assets, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{
		Component: comp,
		Assets: []*core.Asset{{
			Path: rel, Kind: core.AssetContent, BlobDigest: d, Size: n, ContentType: "application/octet-stream",
			Checksums: map[string]string{"sha1": hex.EncodeToString(hs.sha1.Sum(nil)), "sha512": hex.EncodeToString(hs.sha512.Sum(nil))},
		}},
	})
	if err != nil {
		return nil, nil, err
	}
	return assets[0], bs, nil
}
