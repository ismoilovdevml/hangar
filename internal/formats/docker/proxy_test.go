package docker_test

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/docker"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// fakeHub is an in-memory OCI registry standing in for Docker Hub.
type fakeHub struct {
	reg      http.Handler
	down     atomic.Bool
	gets     atomic.Int32 // GET /v2/.../manifests/ requests
	blobGets atomic.Int32 // GET /v2/.../blobs/ requests
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/") {
		f.gets.Add(1)
	}
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
		f.blobGets.Add(1)
	}
	w.Header().Set("RateLimit-Remaining", "99;w=21600")
	f.reg.ServeHTTP(w, r)
}

func TestProxyDockerHub(t *testing.T) {
	tr := newTestReg(t)
	hub := &fakeHub{reg: registry.New(registry.Logger(log.New(io.Discard, "", 0)))}
	upSrv := httptest.NewServer(hub)
	t.Cleanup(upSrv.Close)
	up := &testReg{t: t, srv: upSrv}
	img := newImage("nginx")
	up.pushImage("library/nginx", "latest", img)
	up.pushImage("library/nginx", "1.27", img)

	tr.env.AddRepo(t, &core.Repository{Name: "hub-proxy", Format: "docker", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry-1.docker.io", MetadataTTL: core.Duration(time.Hour)}}})
	tr.env.Upstreams.Set("hub-proxy", hub)
	tr.env.AuthZ.Grant("bob", "repo:hub-proxy:read")
	bob := basic("bob", "secret")

	// Single-segment names get the library/ prefix upstream.
	resp, body := tr.do(http.MethodGet, "/v2/hub-proxy/nginx/manifests/latest", nil, bob)
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(img.manifest) || resp.Header.Get("Docker-Content-Digest") != string(img.digest) {
		t.Fatalf("proxied manifest: %v %s", resp.Header, body)
	}
	if resp.Header.Get("RateLimit-Remaining") == "" {
		t.Fatal("rate-limit headers not passed through")
	}
	layer := "/v2/hub-proxy/nginx/blobs/" + string(core.SHA256Of(img.layer))
	resp, body = tr.do(http.MethodGet, layer, nil, bob)
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(img.layer) {
		t.Fatalf("proxied blob = %q", body)
	}

	// Cached: blobs and fresh tags do not reach the remote again.
	calls := tr.env.Upstreams.Calls("hub-proxy")
	resp, body = tr.do(http.MethodGet, layer, nil, bob)
	tr.expect(resp, body, http.StatusOK)
	resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/nginx/manifests/latest", nil, bob)
	tr.expect(resp, body, http.StatusOK)
	resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/nginx/manifests/"+string(img.digest), nil, bob)
	tr.expect(resp, body, http.StatusOK)
	if got := tr.env.Upstreams.Calls("hub-proxy"); got != calls {
		t.Fatalf("cache hits went upstream: %d -> %d", calls, got)
	}
	proxyRepo := mustRepo(t, tr, "hub-proxy")
	if _, err := tr.env.Components.GetComponent(t.Context(), proxyRepo.ID, "", "nginx", "latest"); err != nil {
		t.Fatalf("cached tag component: %v", err)
	}

	// Stale tag: revalidated with HEAD only (no manifest GET) when unchanged.
	gets := hub.gets.Load()
	tr.now = tr.now.Add(2 * time.Hour)
	resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/nginx/manifests/latest", nil, bob)
	tr.expect(resp, body, http.StatusOK)
	if hub.gets.Load() != gets {
		t.Fatal("unchanged tag re-downloaded")
	}

	// Remote moved the tag: new manifest is fetched.
	img2 := newImage("nginx-2")
	up.pushImage("library/nginx", "latest", img2)
	tr.now = tr.now.Add(2 * time.Hour)
	resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/nginx/manifests/latest", nil, bob)
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(img2.manifest) {
		t.Fatal("moved tag not refreshed")
	}

	// Remote down: stale tag still served; uncached content is a 5xx.
	hub.down.Store(true)
	tr.now = tr.now.Add(2 * time.Hour)
	resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/nginx/manifests/latest", nil, bob)
	tr.expect(resp, body, http.StatusOK)
	resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/nginx/manifests/never-seen", nil, bob)
	if resp.StatusCode < 500 {
		t.Fatalf("uncached manifest with remote down = %d %s", resp.StatusCode, body)
	}
	hub.down.Store(false)

	// Tags list comes from the remote.
	resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/nginx/tags/list", nil, bob)
	tr.expect(resp, body, http.StatusOK)
	if !strings.Contains(string(body), `"1.27"`) {
		t.Fatalf("proxied tags = %s", body)
	}
	// Unknown upstream image.
	resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/nope/manifests/latest", nil, bob)
	tr.expect(resp, body, http.StatusNotFound)

	// Proxies are read-only.
	resp, body = tr.do(http.MethodPost, "/v2/hub-proxy/nginx/blobs/uploads/", nil, alice)
	tr.expect(resp, body, http.StatusMethodNotAllowed)

	// Through a group with a proxy member, using a group-scoped token.
	tr.env.AddRepo(t, &core.Repository{Name: "docker-group", Format: "docker", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"docker-hosted", "hub-proxy"}}}})
	tr.env.AuthZ.Grant("bob", "repo:docker-group:read")
	tok := tr.token("repository:docker-group/nginx:pull", bob)
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/nginx/manifests/1.27", nil, bearer(tok))
	tr.expect(resp, body, http.StatusOK)
	if _, err := tr.env.Components.GetComponent(t.Context(), proxyRepo.ID, "", "nginx", "1.27"); err != nil {
		t.Fatalf("group pull did not fill the proxy cache: %v", err)
	}
}

func TestProxyBlobDedupAcrossRepos(t *testing.T) {
	tr := newTestReg(t)
	hub := &fakeHub{reg: registry.New(registry.Logger(log.New(io.Discard, "", 0)))}
	upSrv := httptest.NewServer(hub)
	t.Cleanup(upSrv.Close)
	tr.env.AddRepo(t, &core.Repository{Name: "ghcr-proxy", Format: "docker", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: upSrv.URL}}})

	// The layer already exists in the shared blob store (pushed to a hosted repo).
	layer := []byte("common layer")
	d := tr.pushBlob("docker-hosted/base", layer, alice)

	// The remote does not serve it under this name: the stored copy must not
	// be handed out (it may belong to a private image).
	resp, body := tr.do(http.MethodGet, "/v2/ghcr-proxy/base/blobs/"+string(d), nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
	if errCode(body) != "BLOB_UNKNOWN" {
		t.Fatalf("code = %s", body)
	}

	// The remote serves it: confirmed with a HEAD only, the bytes are linked
	// from the store instead of being downloaded again.
	up := &testReg{t: t, srv: upSrv}
	up.pushBlob("shared/base", layer)
	resp, body = tr.do(http.MethodGet, "/v2/ghcr-proxy/shared/base/blobs/"+string(d), nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(layer) {
		t.Fatalf("blob = %q", body)
	}
	if n := hub.blobGets.Load(); n != 0 {
		t.Fatalf("blob present in the store was downloaded from the remote (%d GETs)", n)
	}
	calls := tr.env.Upstreams.Calls("ghcr-proxy")
	resp, body = tr.do(http.MethodGet, "/v2/ghcr-proxy/shared/base/blobs/"+string(d), nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if tr.env.Upstreams.Calls("ghcr-proxy") != calls {
		t.Fatal("linked blob is not served from the cache")
	}
}

// A reader holding only the group's read privilege pulls an image through a
// group whose proxy member has never seen it: the proxy cache is filled.
func TestGroupPullFillsProxyWithGroupPrivilegeOnly(t *testing.T) {
	tr := newTestReg(t)
	hub := &fakeHub{reg: registry.New(registry.Logger(log.New(io.Discard, "", 0)))}
	upSrv := httptest.NewServer(hub)
	t.Cleanup(upSrv.Close)
	up := &testReg{t: t, srv: upSrv}
	img := newImage("redis")
	up.pushImage("library/redis", "7", img)

	tr.env.AddRepo(t, &core.Repository{Name: "hub-proxy", Format: "docker", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry-1.docker.io", MetadataTTL: core.Duration(time.Hour)}}})
	tr.env.Upstreams.Set("hub-proxy", hub)
	tr.env.AddRepo(t, &core.Repository{Name: "docker-group", Format: "docker", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"docker-hosted", "hub-proxy"}}}})
	tr.env.Auth.AddUser("carol", "secret")
	tr.env.AuthZ.Grant("carol", "repo:docker-group:read")
	carol := basic("carol", "secret")
	proxyRepo := mustRepo(t, tr, "hub-proxy")

	h0, m0, _ := cacheCounters(t)
	resp, body := tr.do(http.MethodGet, "/v2/docker-group/redis/manifests/7", nil, carol)
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(img.manifest) {
		t.Fatalf("manifest through group = %s", body)
	}
	if _, err := tr.env.Components.GetComponent(t.Context(), proxyRepo.ID, "", "redis", "7"); err != nil {
		t.Fatalf("group pull did not fill the proxy cache: %v", err)
	}
	layer := core.SHA256Of(img.layer)
	tok := tr.token("repository:docker-group/redis:pull", carol)
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/redis/blobs/"+string(layer), nil, bearer(tok))
	tr.expect(resp, body, http.StatusOK)
	if string(body) != string(img.layer) {
		t.Fatalf("layer through group = %q", body)
	}
	if _, err := tr.env.Components.GetAsset(t.Context(), proxyRepo.ID, "redis/blobs/"+string(layer)); err != nil {
		t.Fatalf("group blob pull did not fill the proxy cache: %v", err)
	}
	// Direct access to the member is still refused.
	resp, body = tr.do(http.MethodGet, "/v2/hub-proxy/redis/manifests/7", nil, carol)
	tr.expect(resp, body, http.StatusForbidden)

	// Cache metrics: two misses so far; cached pulls are hits.
	h1, m1, _ := cacheCounters(t)
	if h1-h0 != 0 || m1-m0 != 2 {
		t.Errorf("after first pulls: hits +%v misses +%v, want +0 +2", h1-h0, m1-m0)
	}
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/redis/manifests/7", nil, carol)
	tr.expect(resp, body, http.StatusOK)
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/redis/blobs/"+string(layer), nil, carol)
	tr.expect(resp, body, http.StatusOK)
	h2, m2, _ := cacheCounters(t)
	if h2-h1 != 2 || m2-m1 != 0 {
		t.Errorf("cached pulls: hits +%v misses +%v, want +2 +0", h2-h1, m2-m1)
	}
}

// cacheCounters reads the docker proxy cache request counters.
func cacheCounters(t *testing.T) (hits, misses, hitBytes float64) {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := proxy.RegisterMetrics(reg); err != nil {
		t.Fatal(err)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		for _, m := range f.GetMetric() {
			l := map[string]string{}
			for _, lp := range m.GetLabel() {
				l[lp.GetName()] = lp.GetValue()
			}
			if l["format"] != docker.Name {
				continue
			}
			v := m.GetCounter().GetValue()
			switch {
			case f.GetName() == proxy.MetricCacheHitBytes:
				hitBytes = v
			case l["result"] == proxy.CacheResultHit:
				hits = v
			case l["result"] == proxy.CacheResultMiss:
				misses = v
			}
		}
	}
	return hits, misses, hitBytes
}
