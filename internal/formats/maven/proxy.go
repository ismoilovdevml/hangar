package maven

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// Asset attributes recording the remote validators of a cached document.
const (
	attrRemoteETag         = "remote_etag"
	attrRemoteLastModified = "remote_last_modified"
)

// mutableRemote reports whether a remote file may change under the same path
// and is therefore cached as a metadata asset and revalidated after
// MetadataTTL: maven-metadata.xml, non-unique SNAPSHOT files and anything
// outside the artifact layout. Releases and timestamped snapshots are immutable.
func mutableRemote(mp *mavenPath) bool {
	if mp.kind != kindArtifact {
		return true
	}
	return mp.snapshot() && mp.timestamp == ""
}

// resolveProxy serves from the cache, revalidating per the repository's
// freshness policy (metadata after MetadataTTL; content is immutable unless
// ContentTTL is set), and fetches misses from the remote. Remote 404s are
// negative-cached by the core Upstream. When the remote is unreachable a
// stale copy is served.
func (h *handler) resolveProxy(ctx context.Context, repo *core.Repository, mp *mavenPath) (*entity, error) {
	if err := checkProxy(ctx, repo, mp); err != nil {
		return nil, err
	}
	a, err := h.deps.Components.GetAsset(ctx, repo.ID, mp.path)
	switch {
	case err == nil:
		if proxy.FreshnessOf(repo.Config.Proxy).IsFresh(a.Kind, a.UpdatedAt) {
			proxy.NoteCacheHit(ctx, a.Size)
			return assetEntity(repo, a), nil
		}
		return h.fetchRemote(ctx, repo, mp, a)
	case !errors.Is(err, core.ErrNotFound):
		return nil, err
	}
	return h.fetchRemote(ctx, repo, mp, nil)
}

// fetchRemote downloads mp from the remote (conditionally when cached is
// set), verifies it against the remote's checksum and caches it.
func (h *handler) fetchRemote(ctx context.Context, repo *core.Repository, mp *mavenPath, cached *core.Asset) (*entity, error) {
	stale := func(err error) (*entity, error) {
		if cached != nil {
			h.log.Warn("remote unavailable, serving cached copy", "repository", repo.Name, "path", mp.path, "err", err)
			proxy.NoteCacheHit(ctx, cached.Size)
			return assetEntity(repo, cached), nil
		}
		proxy.NoteCacheMiss(ctx)
		return nil, err
	}
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	req := core.UpstreamRequest{Method: http.MethodGet, Path: mp.path}
	req.Package, req.Index = upstreamPackage(mp)
	if cached != nil {
		req.Conditional.ETag, _ = cached.Attributes[attrRemoteETag].(string)
		if s, ok := cached.Attributes[attrRemoteLastModified].(string); ok {
			req.Conditional.LastModified, _ = time.Parse(time.RFC3339, s)
		}
	}
	resp, err := up.Fetch(ctx, req)
	if err != nil {
		return stale(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.NotModified && cached != nil:
		h.touch(ctx, repo, cached)
		proxy.NoteCacheHit(ctx, cached.Size)
		return assetEntity(repo, cached), nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		proxy.NoteCacheMiss(ctx)
		return nil, fmt.Errorf("%s: not found upstream: %w", mp.path, core.ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return stale(fmt.Errorf("%s: remote answered %d: %w", mp.path, resp.StatusCode, core.ErrUpstream))
	}
	proxy.NoteCacheMiss(ctx)

	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	mh := newMultiHash()
	d, n, err := bs.Put(ctx, io.TeeReader(resp.Body, mh))
	if err != nil {
		return stale(fmt.Errorf("%s: download: %w: %w", mp.path, err, core.ErrUpstream))
	}
	sums := mh.sums()
	if err := h.verifyRemote(ctx, up, mp, resp.Header, sums); err != nil {
		return stale(err)
	}
	asset := &core.Asset{Path: mp.path, Kind: core.AssetContent, BlobDigest: d, ContentType: contentType(mp.path),
		Size: n, Checksums: sums, Attributes: map[string]any{}}
	if mutableRemote(mp) {
		asset.Kind = core.AssetMetadata
	}
	if resp.ETag != "" {
		asset.Attributes[attrRemoteETag] = resp.ETag
	}
	if !resp.LastModified.IsZero() {
		asset.Attributes[attrRemoteLastModified] = resp.LastModified.UTC().Format(time.RFC3339)
	}
	req2 := core.PublishRequest{Assets: []*core.Asset{asset}}
	if mp.kind == kindArtifact {
		maps.Copy(asset.Attributes, artifactAttributes(mp))
		req2.Component = &core.Component{Namespace: mp.groupID, Name: mp.artifactID, Version: mp.version}
	}
	_, stored, err := h.deps.Components.Publish(ctx, repo, req2)
	if err != nil || len(stored) == 0 {
		// Still answer the client from the blob just written; the next
		// request retries the cache write.
		h.log.Warn("proxy cache write failed", "repository", repo.Name, "path", mp.path, "err", err)
		asset.RepositoryID, asset.UpdatedAt = repo.ID, time.Now().UTC()
		return assetEntity(repo, asset), nil
	}
	return assetEntity(repo, stored[0]), nil
}

// touch restarts a cached document's TTL after the remote confirmed it (304).
func (h *handler) touch(ctx context.Context, repo *core.Repository, a *core.Asset) {
	c := *a
	c.ID = ""
	if _, _, err := h.deps.Components.Publish(ctx, repo, core.PublishRequest{Assets: []*core.Asset{&c}}); err != nil {
		h.log.Debug("refresh cached document", "repository", repo.Name, "path", a.Path, "err", err)
	}
}

// verifyRemote compares the downloaded bytes with the checksum the remote
// publishes: the X-Checksum-SHA1/-MD5 response headers when present,
// otherwise the .sha1 side file. A remote without checksums is accepted;
// a mismatch is rejected (corrupt transfer or tampering) and not cached.
func (h *handler) verifyRemote(ctx context.Context, up core.Upstream, mp *mavenPath, hdr http.Header, sums map[string]string) error {
	for _, algo := range []string{"sha1", "md5"} {
		if v := strings.TrimSpace(hdr.Get("X-Checksum-" + algo)); v != "" {
			want, err := parseChecksum(algo, v)
			if err == nil {
				return compareRemote(mp, algo, want, sums[algo])
			}
		}
	}
	pkg, index := upstreamPackage(mp)
	resp, err := up.Fetch(ctx, core.UpstreamRequest{Method: http.MethodGet, Path: mp.path + ".sha1", Package: pkg, Index: index})
	if err != nil {
		h.log.Debug("remote checksum unavailable", "path", mp.path, "err", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumBody))
	if err != nil {
		return nil // an unreadable side file means "no checksum", not a failed download
	}
	want, err := parseChecksum("sha1", string(body))
	if err != nil {
		h.log.Warn("remote published a malformed checksum", "path", mp.path+".sha1")
		return nil
	}
	return compareRemote(mp, "sha1", want, sums["sha1"])
}

func compareRemote(mp *mavenPath, algo, want, got string) error {
	if !strings.EqualFold(want, got) {
		return fmt.Errorf("%s: remote %s %s does not match downloaded content %s: %w", mp.path, algo, want, got, core.ErrUpstream)
	}
	return nil
}
