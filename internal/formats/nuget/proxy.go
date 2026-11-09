package nuget

import (
	"bytes"
	"compress/gzip"
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

// Limits for upstream metadata documents read into memory.
const (
	maxServiceIndex  = 1 << 20
	maxFlatIndex     = 8 << 20
	maxRegistration  = 64 << 20
	maxSearchResults = 16 << 20
)

// upstreamResources are the absolute base URLs found in the upstream service index.
type upstreamResources struct {
	Flat         string // PackageBaseAddress/3.0.0, trailing slash
	Registration string // best registrations hive, trailing slash
	Search       string // SearchQueryService
	// Vulnerability is the VulnerabilityInfo index ("" when the remote has none).
	Vulnerability string
	// Hosts are the hosts of every resource in the service index. The remote
	// declares them, so they are trusted as related hosts of the repository
	// (nuget.org serves search from azuresearch-*.nuget.org).
	Hosts []string
}

type serviceIndexDoc struct {
	Version   string `json:"version"`
	Resources []struct {
		ID   string `json:"@id"`
		Type string `json:"@type"`
	} `json:"resources"`
}

// Registration hives in order of preference: SemVer 2 aware first.
var registrationTypes = []string{
	"RegistrationsBaseUrl/3.6.0", "RegistrationsBaseUrl/3.4.0", "RegistrationsBaseUrl/3.0.0-rc",
	"RegistrationsBaseUrl/3.0.0-beta", "RegistrationsBaseUrl",
}

var searchTypes = []string{
	"SearchQueryService/3.5.0", "SearchQueryService/3.0.0-rc", "SearchQueryService/3.0.0-beta", "SearchQueryService",
}

func parseServiceIndex(b []byte) (*upstreamResources, error) {
	var doc serviceIndexDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("upstream service index: %w: %w", err, core.ErrUpstream)
	}
	find := func(types ...string) string {
		for _, t := range types {
			for _, r := range doc.Resources {
				if r.Type == t && r.ID != "" {
					return r.ID
				}
			}
		}
		return ""
	}
	slash := func(s string) string {
		if s != "" && !strings.HasSuffix(s, "/") {
			s += "/"
		}
		return s
	}
	res := &upstreamResources{
		Flat:          slash(find("PackageBaseAddress/3.0.0")),
		Registration:  slash(find(registrationTypes...)),
		Search:        find(searchTypes...),
		Vulnerability: find(vulnerabilityType),
	}
	if res.Flat == "" {
		return nil, fmt.Errorf("upstream service index has no PackageBaseAddress/3.0.0: %w", core.ErrUpstream)
	}
	seen := map[string]bool{}
	for _, r := range doc.Resources {
		if u, err := url.Parse(r.ID); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil {
			if h := strings.ToLower(u.Host); !seen[h] {
				seen[h] = true
				res.Hosts = append(res.Hosts, h)
			}
		}
	}
	return res, nil
}

func (h *handler) upstreamIndex(ctx context.Context, repo *core.Repository) (*upstreamResources, error) {
	cfg, err := decodeConfig(repo)
	if err != nil {
		return nil, err
	}
	b, err := h.cachedDoc(ctx, repo, upstreamPrefix+"index.json", func(ctx context.Context) ([]byte, error) {
		return h.fetchDoc(ctx, repo, h.remoteBase(repo)+"/"+cfg.ServiceIndex, "", maxServiceIndex)
	})
	if err != nil {
		// A remote without a service index is broken (502); an offline-mode
		// repository that never cached one has nothing to serve (404).
		if errors.Is(err, core.ErrNotFound) && !errors.Is(err, core.ErrUpstream) {
			return nil, fmt.Errorf("upstream service index not found: %w", core.ErrUpstream)
		}
		return nil, err
	}
	return parseServiceIndex(b)
}

func (h *handler) remoteBase(repo *core.Repository) string {
	return strings.TrimSuffix(repo.Config.Proxy.RemoteURL, "/")
}

// fetch GETs an absolute upstream URL through the repository's Upstream
// (SSRF guard, circuit breaker, request collapsing, outbound proxy, rate
// limits). URLs under the remote URL are remote paths and get the remote's
// credentials; resources the service index places on other hosts (search,
// registrations, CDNs) are fetched as related hosts, without them. Hosts
// neither in the service index nor in the repository's related_hosts are
// refused. lid is the package the request is about, "" for documents about
// no single package (service index, search); routing rules see it.
func (h *handler) fetch(ctx context.Context, repo *core.Repository, abs, lid string) (*core.UpstreamResponse, error) {
	req := core.UpstreamRequest{Header: http.Header{"Accept": {"application/json, */*"}}, Package: lid, Index: lid == ""}
	if rel, ok := strings.CutPrefix(abs, h.remoteBase(repo)+"/"); ok {
		req.Path = rel
	} else {
		// The service index itself is under the remote URL, so this lookup
		// never recurses.
		idx, err := h.upstreamIndex(ctx, repo)
		if err != nil {
			return nil, err
		}
		req.URL, req.RelatedHosts = abs, idx.Hosts
	}
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	resp, err := up.Fetch(ctx, req)
	if errors.Is(err, core.ErrInvalid) && req.URL != "" {
		return nil, fmt.Errorf("upstream resource %s is outside the remote and its related hosts: %w", abs, core.ErrUpstream)
	}
	return resp, err
}

// fetchDoc fetches a JSON document of at most limit bytes. 404 is ErrNotFound.
func (h *handler) fetchDoc(ctx context.Context, repo *core.Repository, abs, lid string, limit int64) ([]byte, error) {
	resp, err := h.fetch(ctx, repo, abs, lid)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("upstream %s: %w", abs, core.ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("upstream %s: status %d: %w", abs, resp.StatusCode, core.ErrUpstream)
	}
	var body io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("upstream %s: %w: %w", abs, err, core.ErrUpstream)
		}
		defer zr.Close()
		body = zr
	}
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read upstream %s: %w: %w", abs, err, core.ErrUpstream)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("upstream %s: document larger than %d bytes: %w", abs, limit, core.ErrUpstream)
	}
	return b, nil
}

// cachedDoc serves a metadata document from the proxy cache while it is
// younger than the metadata TTL, otherwise rebuilds it with build and stores
// it. When the upstream fails, a stale copy is served.
func (h *handler) cachedDoc(ctx context.Context, repo *core.Repository, path string, build func(context.Context) ([]byte, error)) ([]byte, error) {
	asset, err := h.deps.Components.GetAsset(ctx, repo.ID, path)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	if asset != nil && h.now().Sub(asset.UpdatedAt) < metadataTTL(repo.Config.Proxy) {
		if b, err := h.readAsset(ctx, repo, asset); err == nil {
			proxy.NoteCacheHit(ctx, asset.Size)
			return b, nil
		}
	}
	b, err := build(ctx)
	if err != nil {
		if asset != nil && !errors.Is(err, core.ErrNotFound) {
			if stale, rerr := h.readAsset(ctx, repo, asset); rerr == nil {
				h.log().WarnContext(ctx, "nuget upstream failed; serving stale metadata", "repo", repo.Name, "path", path, "err", err)
				proxy.NoteCacheHit(ctx, asset.Size)
				return stale, nil
			}
		}
		proxy.NoteCacheMiss(ctx)
		return nil, err
	}
	proxy.NoteCacheMiss(ctx)
	if err := h.storeMetadata(ctx, repo, path, b); err != nil {
		h.log().WarnContext(ctx, "nuget cache store failed", "repo", repo.Name, "path", path, "err", err)
	}
	return b, nil
}

func (h *handler) storeMetadata(ctx context.Context, repo *core.Repository, path string, b []byte) error {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return err
	}
	d, n, err := bs.Put(ctx, bytes.NewReader(b))
	if err != nil {
		return err
	}
	_, _, err = h.deps.Components.Publish(ctx, repo, core.PublishRequest{Assets: []*core.Asset{{
		Path: path, Kind: core.AssetMetadata, BlobDigest: d, Size: n, ContentType: contentTypeJSON,
	}}})
	return err
}

func (h *handler) readAsset(ctx context.Context, repo *core.Repository, a *core.Asset) ([]byte, error) {
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	rc, err := bs.Get(ctx, a.BlobDigest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// proxyVersions returns the upstream flat container version list.
func (h *handler) proxyVersions(ctx context.Context, repo *core.Repository, lid string) ([]Version, error) {
	b, err := h.cachedDoc(ctx, repo, upstreamPrefix+"flat/"+lid+"/index.json", func(ctx context.Context) ([]byte, error) {
		idx, err := h.upstreamIndex(ctx, repo)
		if err != nil {
			return nil, err
		}
		return h.fetchDoc(ctx, repo, idx.Flat+lid+"/index.json", lid, maxFlatIndex)
	})
	if err != nil {
		return nil, err
	}
	var doc struct {
		Versions []string `json:"versions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("upstream version list of %q: %w: %w", lid, err, core.ErrUpstream)
	}
	out := make([]Version, 0, len(doc.Versions))
	for _, s := range doc.Versions {
		if v, err := ParseVersion(s); err == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

type upstreamPage struct {
	ID    string `json:"@id"`
	Items []struct {
		CatalogEntry json.RawMessage `json:"catalogEntry"`
	} `json:"items"`
}

// proxyRegistration returns the upstream catalog entries of lid, following
// non-inlined registration pages. The assembled list is cached as one document.
func (h *handler) proxyRegistration(ctx context.Context, repo *core.Repository, lid string) ([]map[string]any, error) {
	b, err := h.cachedDoc(ctx, repo, upstreamPrefix+"registration/"+lid+".json", func(ctx context.Context) ([]byte, error) {
		idx, err := h.upstreamIndex(ctx, repo)
		if err != nil {
			return nil, err
		}
		if idx.Registration == "" {
			return nil, fmt.Errorf("upstream has no registration resource: %w", core.ErrNotFound)
		}
		doc, err := h.fetchDoc(ctx, repo, idx.Registration+lid+"/index.json", lid, maxRegistration)
		if err != nil {
			return nil, err
		}
		var index struct {
			Items []upstreamPage `json:"items"`
		}
		if err := json.Unmarshal(doc, &index); err != nil {
			return nil, fmt.Errorf("upstream registration of %q: %w: %w", lid, err, core.ErrUpstream)
		}
		var cats []json.RawMessage
		for _, p := range index.Items {
			if p.Items == nil && p.ID != "" {
				pb, err := h.fetchDoc(ctx, repo, p.ID, lid, maxRegistration)
				if err != nil {
					return nil, err
				}
				p = upstreamPage{}
				if err := json.Unmarshal(pb, &p); err != nil {
					return nil, fmt.Errorf("upstream registration page: %w: %w", err, core.ErrUpstream)
				}
			}
			for _, it := range p.Items {
				if len(it.CatalogEntry) > 0 && it.CatalogEntry[0] == '{' {
					cats = append(cats, it.CatalogEntry)
				}
			}
		}
		return json.Marshal(cats)
	})
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("cached registration of %q: %w", lid, err)
	}
	return out, nil
}

// DefaultMetadataTTL applies when a proxy leaves MetadataTTL at zero; a
// negative MetadataTTL revalidates on every request.
const DefaultMetadataTTL = time.Hour

func metadataTTL(cfg *core.ProxyConfig) time.Duration {
	if cfg == nil || cfg.MetadataTTL == 0 {
		return DefaultMetadataTTL
	}
	return cfg.MetadataTTL.D()
}

// cachePackage downloads lid/lver from the upstream flat container and
// stores it like a pushed package, published as the reader that asked for it
// (a proxy cache fill needs read access to the proxy, or to the group the
// reader came through). Packages are immutable: once cached they are never
// fetched again. When the reader may not fill the cache, the package is
// served without being recorded: the returned assets point at the stored
// blobs but are not in the repository.
func (h *handler) cachePackage(ctx context.Context, repo *core.Repository, lid, lver string) ([]*core.Asset, error) {
	idx, err := h.upstreamIndex(ctx, repo)
	if err != nil {
		return nil, err
	}
	abs := idx.Flat + lid + "/" + lver + "/" + lid + "." + lver + ".nupkg"
	resp, err := h.fetch(ctx, repo, abs, lid)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("upstream %s: %w", abs, core.ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("upstream %s: status %d: %w", abs, resp.StatusCode, core.ErrUpstream)
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	up, err := h.putPackage(ctx, bs, resp.Body)
	if err != nil {
		return nil, err
	}
	md, assets, err := h.storePackage(ctx, repo, bs, up)
	if err != nil {
		if assets != nil && (errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrUnauthenticated)) {
			h.log().WarnContext(ctx, "nuget package served without caching", "repo", repo.Name, "package", lid+"/"+lver, "err", err)
			return assets, nil
		}
		return nil, fmt.Errorf("cache %s: %w", abs, err)
	}
	if lowerID(md.ID) != lid {
		h.log().WarnContext(ctx, "nuget upstream package id differs from request", "repo", repo.Name, "want", lid, "got", md.ID)
	}
	return nil, nil
}
