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

	"github.com/ismoilovdevml/hangar/internal/core"
)

// fakeHub is an in-memory OCI registry standing in for Docker Hub.
type fakeHub struct {
	reg  http.Handler
	down atomic.Bool
	gets atomic.Int32 // GET /v2/.../manifests/ requests
}

func (f *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/") {
		f.gets.Add(1)
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
	d := tr.pushBlob("docker-hosted/base", []byte("common layer"), alice)
	resp, body := tr.do(http.MethodGet, "/v2/ghcr-proxy/base/blobs/"+string(d), nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if tr.env.Upstreams.Calls("ghcr-proxy") != 0 {
		t.Fatal("blob present in the store was fetched from the remote")
	}
}
