package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// proxyBlobTimeout bounds a proxied blob download that continues to fill the
// cache after the client disconnected.
const proxyBlobTimeout = 30 * time.Minute

// dockerHubHosts are the registry hosts that need the "library/" prefix for
// official images ("nginx" -> "library/nginx").
var dockerHubHosts = map[string]bool{
	"registry-1.docker.io":    true,
	"docker.io":               true,
	"index.docker.io":         true,
	"registry.hub.docker.com": true,
}

// rateLimitHeaders are passed from the remote to the client so users see
// Docker Hub pull limits.
var rateLimitHeaders = []string{"RateLimit-Limit", "RateLimit-Remaining", "Docker-RateLimit-Source"}

// upstreamName maps an image path to the remote's repository name.
func upstreamName(repo *core.Repository, image string) string {
	if strings.Contains(image, "/") || repo.Config.Proxy == nil {
		return image
	}
	if u, err := url.Parse(repo.Config.Proxy.RemoteURL); err == nil && dockerHubHosts[strings.ToLower(u.Hostname())] {
		return "library/" + image
	}
	return image
}

func copyRateLimit(dst, src http.Header) {
	if dst == nil {
		return
	}
	for _, k := range rateLimitHeaders {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func closeBody(resp *core.UpstreamResponse) {
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
}

// upstreamStatusErr turns an unexpected remote status into an error.
func upstreamStatusErr(what string, status int) error {
	if status == http.StatusNotFound {
		return fmt.Errorf("%s: remote answered 404: %w", what, core.ErrNotFound)
	}
	return fmt.Errorf("%s: remote answered %d: %w", what, status, core.ErrUpstream)
}

func (h *handler) fetch(ctx context.Context, repo *core.Repository, method, path, accept string) (*core.UpstreamResponse, error) {
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	req := core.UpstreamRequest{Method: method, Path: path}
	if accept != "" {
		req.Header = http.Header{"Accept": []string{accept}}
	}
	return up.Fetch(ctx, req)
}

// proxyManifest serves image:ref from a proxy repository's cache, revalidating
// tags older than the metadata TTL with a HEAD request (which Docker Hub does
// not count against pull limits) and falling back to the stale copy when the
// remote is unreachable. hdr receives the remote's rate-limit headers.
func (h *handler) proxyManifest(ctx context.Context, hdr http.Header, repo *core.Repository, image, ref string) (*manifestHit, error) {
	local, err := h.localManifest(ctx, repo, image, ref)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	if local != nil && (isDigest(ref) || h.fresh(repo, local.asset)) {
		return local, nil
	}
	path := "v2/" + upstreamName(repo, image) + "/manifests/" + ref
	if local != nil {
		resp, err := h.fetch(ctx, repo, http.MethodHead, path, acceptManifests)
		if err != nil {
			return local, nil // remote down: serve stale
		}
		closeBody(resp)
		copyRateLimit(hdr, resp.Header)
		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, fmt.Errorf("manifest %s:%s gone upstream: %w", image, ref, core.ErrNotFound)
		case resp.StatusCode != http.StatusOK:
			return local, nil
		case resp.Header.Get("Docker-Content-Digest") == string(local.digest):
			h.touchTag(ctx, repo, image, ref, local)
			return local, nil
		}
	}
	resp, err := h.fetch(ctx, repo, http.MethodGet, path, acceptManifests)
	if err != nil {
		if local != nil {
			return local, nil
		}
		return nil, err
	}
	defer closeBody(resp)
	copyRateLimit(hdr, resp.Header)
	if resp.StatusCode != http.StatusOK {
		if local != nil && resp.StatusCode != http.StatusNotFound {
			return local, nil
		}
		return nil, upstreamStatusErr("manifest "+image+":"+ref, resp.StatusCode)
	}
	body, err := readManifestBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("manifest %s:%s from remote: %w: %w", image, ref, err, core.ErrUpstream)
	}
	ct := resp.Header.Get("Content-Type")
	if parsed, _, err := mime.ParseMediaType(ct); err == nil {
		ct = parsed
	}
	m, mt, err := parseManifest(body, ct)
	if err != nil {
		return nil, fmt.Errorf("manifest %s:%s from remote: %w: %w", image, ref, err, core.ErrUpstream)
	}
	d := core.SHA256Of(body)
	tag := ref
	if isDigest(ref) {
		want := core.Digest(ref)
		hs := want.NewDigester()
		if hs == nil {
			return nil, fmt.Errorf("digest %s: %w", ref, core.ErrInvalid)
		}
		hs.Write(body)
		if got := digestOf(want.Algorithm(), hs.Sum(nil)); got != want {
			return nil, fmt.Errorf("remote manifest digest %s, want %s: %w", got, want, core.ErrUpstream)
		}
		d, tag = want, ""
	} else if hd := resp.Header.Get("Docker-Content-Digest"); hd != "" && strings.HasPrefix(hd, core.AlgorithmSHA256+":") && core.Digest(hd) != d {
		return nil, fmt.Errorf("remote manifest digest header %s, body %s: %w", hd, d, core.ErrUpstream)
	}
	a, err := h.storeManifest(ctx, repo, image, body, mt, d, m, tag, true)
	if err != nil {
		return nil, err
	}
	return &manifestHit{repo: repo, asset: a, digest: d, mediaType: mt}, nil
}

// fresh reports whether a cached tag may be served without revalidation
// (tags are metadata: they move upstream).
func (h *handler) fresh(repo *core.Repository, a *core.Asset) bool {
	return proxy.FreshnessOf(repo.Config.Proxy).IsFreshAt(core.AssetMetadata, a.UpdatedAt, h.now())
}

// touchTag re-publishes an unchanged cached tag so its age restarts.
func (h *handler) touchTag(ctx context.Context, repo *core.Repository, image, tag string, hit *manifestHit) {
	ns, name := splitImage(image)
	a := *hit.asset
	a.ID = ""
	h.cacheFill(ctx, repo, core.PublishRequest{
		Component: &core.Component{Namespace: ns, Name: name, Version: tag},
		Assets:    []*core.Asset{&a},
	})
}

// proxyBlob serves a blob missing from a proxy repository's cache: from the
// shared blob store if another repository already holds the bytes, otherwise
// streamed from the remote to the client while it is written to the store.
// served reports whether a response was written.
func (h *handler) proxyBlob(w http.ResponseWriter, r *http.Request, repo *core.Repository, image string, d core.Digest) (served bool, err error) {
	ctx := r.Context()
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return false, err
	}
	if d.Algorithm() == core.AlgorithmSHA256 {
		if info, err := bs.Stat(ctx, d); err == nil {
			a := h.cacheFill(ctx, repo, core.PublishRequest{Assets: []*core.Asset{blobAsset(image, d, d, info.Size)}})[0]
			h.serveAsset(w, r, repo, a, d, a.ContentType)
			return true, nil
		}
	}
	path := "v2/" + upstreamName(repo, image) + "/blobs/" + string(d)
	if r.Method == http.MethodHead {
		resp, err := h.fetch(ctx, repo, http.MethodHead, path, "")
		if err != nil {
			return false, err
		}
		closeBody(resp)
		copyRateLimit(w.Header(), resp.Header)
		if resp.StatusCode != http.StatusOK {
			return false, upstreamStatusErr("blob "+string(d), resp.StatusCode)
		}
		hd := w.Header()
		hd.Set("Content-Type", "application/octet-stream")
		hd.Set("Docker-Content-Digest", string(d))
		if resp.ContentLength >= 0 {
			hd.Set("Content-Length", sizeString(resp.ContentLength))
		}
		w.WriteHeader(http.StatusOK)
		return true, nil
	}

	// Keep filling the cache if the client goes away mid-download.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), proxyBlobTimeout)
	defer cancel()
	resp, err := h.fetch(fctx, repo, http.MethodGet, path, "")
	if err != nil {
		return false, err
	}
	defer closeBody(resp)
	copyRateLimit(w.Header(), resp.Header)
	if resp.StatusCode != http.StatusOK {
		return false, upstreamStatusErr("blob "+string(d), resp.StatusCode)
	}
	hs := d.NewDigester()
	if hs == nil {
		return false, fmt.Errorf("digest %s: %w", d, core.ErrInvalid)
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("Docker-Content-Digest", string(d))
	if resp.ContentLength >= 0 {
		hd.Set("Content-Length", sizeString(resp.ContentLength))
	}
	w.WriteHeader(http.StatusOK)
	stored, n, err := bs.Put(fctx, io.TeeReader(resp.Body, io.MultiWriter(hs, &bestEffortWriter{w: w})))
	if err != nil {
		h.log.WarnContext(ctx, "docker: proxy blob download failed", "repository", repo.Name, "digest", d, "err", err)
		return true, nil
	}
	if got := digestOf(d.Algorithm(), hs.Sum(nil)); got != d || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		h.log.ErrorContext(ctx, "docker: remote blob does not match its digest", "repository", repo.Name, "digest", d, "got", got)
		return true, nil
	}
	a := h.cacheFill(fctx, repo, core.PublishRequest{Assets: []*core.Asset{blobAsset(image, d, stored, n)}})[0]
	if a.ID != "" {
		h.deps.Components.RecordDownload(ctx, a)
	}
	return true, nil
}

// proxyTags lists the remote's tags (falling back to the cache).
func (h *handler) proxyTags(ctx context.Context, hdr http.Header, repo *core.Repository, image string) ([]string, error) {
	local, err := h.localTags(ctx, repo, image)
	if err != nil {
		return nil, err
	}
	resp, err := h.fetch(ctx, repo, http.MethodGet, "v2/"+upstreamName(repo, image)+"/tags/list", "application/json")
	if err != nil {
		return local, nil
	}
	defer closeBody(resp)
	copyRateLimit(hdr, resp.Header)
	if resp.StatusCode != http.StatusOK {
		return local, nil
	}
	var tl struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&tl); err != nil {
		return local, nil
	}
	return append(local, tl.Tags...), nil
}

// proxyReferrers asks the remote for referrers (falling back to the cache).
func (h *handler) proxyReferrers(ctx context.Context, repo *core.Repository, image string, subject core.Digest) []descriptor {
	resp, err := h.fetch(ctx, repo, http.MethodGet, "v2/"+upstreamName(repo, image)+"/referrers/"+string(subject), mediaOCIIndex)
	if err != nil {
		return nil
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var idx manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifestSize)).Decode(&idx); err != nil {
		return nil
	}
	return idx.Manifests
}
