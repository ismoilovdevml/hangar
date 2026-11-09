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
	"slices"
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

// fetch sends one request to repo's remote about the image remoteName (the
// routing identity; the token dance of the remote runs inside Fetch).
func (h *handler) fetch(ctx context.Context, repo *core.Repository, method, path, accept, remoteName string) (*core.UpstreamResponse, error) {
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	req := core.UpstreamRequest{Method: method, Path: path, Package: remoteName}
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
	// fromCache answers with the cached manifest (fresh, confirmed by the
	// remote, or stale while the remote fails).
	fromCache := func() (*manifestHit, error) {
		proxy.NoteCacheHit(ctx, local.asset.Size)
		return local, nil
	}
	if local != nil && (isDigest(ref) || h.fresh(repo, local.asset)) {
		return fromCache()
	}
	remoteName := upstreamName(repo, image)
	if local == nil && h.unknown.unknown(repo, remoteName, h.now()) {
		proxy.NoteCacheMiss(ctx)
		return nil, fmt.Errorf("manifest %s:%s: %w", image, ref, errNameUnknownCached)
	}
	path := "v2/" + remoteName + "/manifests/" + ref
	if local != nil {
		resp, err := h.fetch(ctx, repo, http.MethodHead, path, acceptManifests, remoteName)
		if err != nil {
			return fromCache() // remote down: serve stale
		}
		closeBody(resp)
		copyRateLimit(hdr, resp.Header)
		switch {
		case resp.StatusCode == http.StatusNotFound:
			proxy.NoteCacheMiss(ctx)
			return nil, fmt.Errorf("manifest %s:%s gone upstream: %w", image, ref, core.ErrNotFound)
		case resp.StatusCode != http.StatusOK:
			return fromCache()
		case resp.Header.Get("Docker-Content-Digest") == string(local.digest):
			h.touchTag(ctx, repo, image, ref, local)
			return fromCache()
		}
	}
	resp, err := h.fetch(ctx, repo, http.MethodGet, path, acceptManifests, remoteName)
	if err != nil {
		if local != nil {
			return fromCache()
		}
		proxy.NoteCacheMiss(ctx)
		return nil, err
	}
	defer closeBody(resp)
	copyRateLimit(hdr, resp.Header)
	if resp.StatusCode != http.StatusOK {
		if local != nil && resp.StatusCode != http.StatusNotFound {
			return fromCache()
		}
		proxy.NoteCacheMiss(ctx)
		return nil, h.remoteMiss(repo, remoteName, "manifest "+image+":"+ref, resp)
	}
	proxy.NoteCacheMiss(ctx)
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

// proxyBlob serves a blob missing from a proxy repository's cache, streamed
// from the remote to the client while it is written to the store. When the
// shared blob store already holds the bytes (another repository or image has
// them), the remote is only asked whether it serves the blob under this name
// (a HEAD, which Docker Hub does not count as a pull) and the stored copy is
// linked. Without that confirmation a proxy would hand out any stored blob to
// anyone who knows its digest, including blobs of private or quarantined
// images, and a push through a group would see layers "exist" that its write
// member does not have. served reports whether a response was written.
func (h *handler) proxyBlob(w http.ResponseWriter, r *http.Request, repo *core.Repository, image string, d core.Digest) (served bool, err error) {
	ctx := r.Context()
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return false, err
	}
	remoteName := upstreamName(repo, image)
	if h.unknown.unknown(repo, remoteName, h.now()) {
		proxy.NoteCacheMiss(ctx)
		return false, fmt.Errorf("blob %s: %w", d, errNameUnknownCached)
	}
	path := "v2/" + remoteName + "/blobs/" + string(d)
	if d.Algorithm() == core.AlgorithmSHA256 {
		if info, err := bs.Stat(ctx, d); err == nil {
			if err := h.remoteHasBlob(ctx, w.Header(), repo, remoteName, path, d); err != nil {
				proxy.NoteCacheMiss(ctx)
				return false, err
			}
			proxy.NoteCacheHit(ctx, info.Size)
			a := h.cacheFill(ctx, repo, core.PublishRequest{Assets: []*core.Asset{blobAsset(image, d, d, info.Size)}})[0]
			h.serveAsset(w, r, repo, a, d, a.ContentType)
			return true, nil
		}
	}
	proxy.NoteCacheMiss(ctx)
	if r.Method == http.MethodHead {
		resp, err := h.fetch(ctx, repo, http.MethodHead, path, "", remoteName)
		if err != nil {
			return false, err
		}
		closeBody(resp)
		copyRateLimit(w.Header(), resp.Header)
		if resp.StatusCode != http.StatusOK {
			return false, h.remoteMiss(repo, remoteName, "blob "+string(d), resp)
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
	resp, err := h.fetch(fctx, repo, http.MethodGet, path, "", remoteName)
	if err != nil {
		return false, err
	}
	defer closeBody(resp)
	copyRateLimit(w.Header(), resp.Header)
	if resp.StatusCode != http.StatusOK {
		return false, h.remoteMiss(repo, remoteName, "blob "+string(d), resp)
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
	if a.ID != "" && !isScanner(ctx) {
		h.deps.Components.RecordDownload(ctx, a)
	}
	return true, nil
}

// remoteHasBlob confirms with a HEAD that the remote serves blob d under
// remoteName. It returns nil if it does, ErrNotFound if it does not, and
// ErrUpstream if the remote cannot be asked.
func (h *handler) remoteHasBlob(ctx context.Context, hdr http.Header, repo *core.Repository, remoteName, path string, d core.Digest) error {
	resp, err := h.fetch(ctx, repo, http.MethodHead, path, "", remoteName)
	if err != nil {
		return err
	}
	closeBody(resp)
	copyRateLimit(hdr, resp.Header)
	if resp.StatusCode != http.StatusOK {
		return h.remoteMiss(repo, remoteName, "blob "+string(d), resp)
	}
	return nil
}

// maxTagPages bounds how many pages of a remote's tag list are followed.
const maxTagPages = 200

// proxyTags lists the remote's tags merged with the cached ones (only the
// cached ones when the remote cannot be reached). known reports whether the
// image exists: cached tags, or a remote that listed it. A remote that pages
// its tag list (Link: <...>; rel="next", as Quay, Harbor and GitLab do) is
// followed to the last page; when a later page fails, the tags read so far
// are kept.
func (h *handler) proxyTags(ctx context.Context, hdr http.Header, repo *core.Repository, image string) (tags []string, known bool, err error) {
	local, err := h.localTags(ctx, repo, image)
	if err != nil {
		return nil, false, err
	}
	known = len(local) > 0
	remoteName := upstreamName(repo, image)
	if h.unknown.unknown(repo, remoteName, h.now()) {
		return local, known, nil
	}
	path := "v2/" + remoteName + "/tags/list"
	seen := map[string]bool{}
	var remote []string
	for page := 0; path != ""; page++ {
		if page == maxTagPages || seen[path] {
			h.log.WarnContext(ctx, "docker: remote tag list pages stopped", "repository", repo.Name, "image", remoteName, "pages", page)
			break
		}
		seen[path] = true
		resp, err := h.fetch(ctx, repo, http.MethodGet, path, "application/json", remoteName)
		if err != nil {
			if page > 0 {
				h.log.WarnContext(ctx, "docker: remote tag list page failed", "repository", repo.Name, "image", remoteName, "page", page, "err", err)
				break
			}
			// Remote down: an image we cannot rule out is reported as known.
			return local, true, nil
		}
		copyRateLimit(hdr, resp.Header)
		if resp.StatusCode != http.StatusOK {
			closeBody(resp)
			if page > 0 {
				h.log.WarnContext(ctx, "docker: remote tag list page failed", "repository", repo.Name, "image", remoteName, "page", page, "status", resp.StatusCode)
				break
			}
			if err := h.remoteMiss(repo, remoteName, "tags of "+image, resp); !errors.Is(err, core.ErrNotFound) {
				known = true // remote failing: do not claim the name is unknown
			}
			return local, known, nil
		}
		var tl struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&tl)
		closeBody(resp)
		if err != nil {
			if page > 0 {
				break
			}
			return local, true, nil
		}
		remote = append(remote, tl.Tags...)
		path = nextTagPage(repo.Config.Proxy.RemoteURL, path, resp.Header.Get("Link"))
	}
	return append(local, remote...), true, nil
}

// nextTagPage resolves the rel="next" target of a tag list page fetched
// from path (relative to remoteURL) to the next path relative to remoteURL,
// or "" when there is none or it points away from the remote.
func nextTagPage(remoteURL, path, link string) string {
	target := ""
	for _, part := range strings.Split(link, ",") {
		ref, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		ref = strings.TrimSpace(ref)
		if !ok || !strings.HasPrefix(ref, "<") || !strings.HasSuffix(ref, ">") {
			continue
		}
		for _, p := range strings.Split(params, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if strings.EqualFold(strings.TrimSpace(k), "rel") && slices.Contains(strings.Fields(strings.ToLower(strings.Trim(strings.TrimSpace(v), `"`))), "next") {
				target = ref[1 : len(ref)-1]
			}
		}
		if target != "" {
			break
		}
	}
	if target == "" {
		return ""
	}
	base, err := url.Parse(strings.TrimSuffix(remoteURL, "/") + "/")
	if err != nil {
		return ""
	}
	cur, err := base.Parse(path)
	if err != nil {
		return ""
	}
	next, err := cur.Parse(target)
	if err != nil || !strings.EqualFold(next.Scheme, base.Scheme) || !strings.EqualFold(next.Host, base.Host) {
		return ""
	}
	rel, ok := strings.CutPrefix(next.EscapedPath(), base.EscapedPath())
	if !ok || rel == "" {
		return ""
	}
	if next.RawQuery != "" {
		rel += "?" + next.RawQuery
	}
	return rel
}

// proxyReferrers asks the remote for referrers (falling back to the cache).
func (h *handler) proxyReferrers(ctx context.Context, repo *core.Repository, image string, subject core.Digest) []descriptor {
	remoteName := upstreamName(repo, image)
	resp, err := h.fetch(ctx, repo, http.MethodGet, "v2/"+remoteName+"/referrers/"+string(subject), mediaOCIIndex, remoteName)
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
