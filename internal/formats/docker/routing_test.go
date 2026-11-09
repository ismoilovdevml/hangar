package docker_test

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// pathLog wraps a registry and remembers the request paths it saw.
type pathLog struct {
	h     http.Handler
	mu    sync.Mutex
	paths []string
}

func (p *pathLog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	p.h.ServeHTTP(w, r)
}

func (p *pathLog) saw(sub string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.paths {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// routingReg: docker-hosted holds edcom/api:1.0; the public remote of
// docker-proxy (a real HTTP registry) holds a different edcom/api:1.0, an
// edcom/api:9.9 and public/tool:1; docker-group = [docker-hosted, docker-proxy].
func routingReg(t *testing.T) (*testReg, *pathLog, testImage, testImage) {
	t.Helper()
	tr := newTestReg(t)
	remote := &pathLog{h: registry.New(registry.Logger(log.New(io.Discard, "", 0)))}
	srv := httptest.NewServer(remote)
	t.Cleanup(srv.Close)
	up := &testReg{t: t, srv: srv}
	internal, impostor := newImage("internal"), newImage("impostor")
	up.pushImage("edcom/api", "1.0", impostor)
	up.pushImage("edcom/api", "9.9", impostor)
	up.pushImage("public/tool", "1", newImage("tool"))
	tr.pushImage("docker-hosted/edcom/api", "1.0", internal, alice)
	tr.env.AddRepo(t, &core.Repository{Name: "docker-proxy", Format: "docker", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: srv.URL, MetadataTTL: core.Duration(time.Hour)}}})
	tr.env.AddRepo(t, &core.Repository{Name: "docker-group", Format: "docker", Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"docker-hosted", "docker-proxy"}}}})
	remote.mu.Lock()
	remote.paths = nil
	remote.mu.Unlock()
	return tr, remote, internal, impostor
}

func setRouting(t *testing.T, tr *testReg, name string, rules *core.RoutingRules) {
	t.Helper()
	r := mustRepo(t, tr, name)
	r.Config.Routing = rules
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	tr.env.Repos.Put(r)
}

func TestRoutingGroup(t *testing.T) {
	tr, _, internal, impostor := routingReg(t)
	// Unprotected: a tag only the public remote has is served through the group.
	resp, body := tr.do(http.MethodGet, "/v2/docker-group/edcom/api/manifests/9.9", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if resp.Header.Get("Docker-Content-Digest") != string(impostor.digest) {
		t.Fatal("unprotected group did not serve the public tag")
	}

	setRouting(t, tr, "docker-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "docker-hosted", Exclusive: []string{"edcom/*"}}}})
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/edcom/api/manifests/9.9", nil, alice)
	if resp.StatusCode != http.StatusNotFound || errCode(body) != "MANIFEST_UNKNOWN" ||
		!strings.Contains(resp.Header.Get(routing.Header), `"edcom/*"`) || !strings.Contains(string(body), "routing rule") {
		t.Errorf("protected public tag = %d %q %s", resp.StatusCode, resp.Header.Get(routing.Header), body)
	}
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/edcom/api/manifests/1.0", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if resp.Header.Get("Docker-Content-Digest") != string(internal.digest) {
		t.Error("group did not serve the hosted image")
	}
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/edcom/api/blobs/"+string(core.SHA256Of(impostor.layer)), nil, alice)
	if resp.StatusCode != http.StatusNotFound || errCode(body) != "BLOB_UNKNOWN" {
		t.Errorf("public layer through group = %d %s", resp.StatusCode, body)
	}
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/edcom/api/tags/list", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if strings.Contains(string(body), "9.9") || !strings.Contains(string(body), "1.0") {
		t.Errorf("protected tags = %s", body)
	}
	// Unclaimed names still come from the proxy.
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/public/tool/manifests/1", nil, alice)
	tr.expect(resp, body, http.StatusOK)

	// Every member excluded: NAME_UNKNOWN naming the rule.
	setRouting(t, tr, "docker-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "docker-hosted", Block: []string{"edcom/*"}}, {Member: "docker-proxy", Block: []string{"edcom/*"}}}})
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/edcom/api/tags/list", nil, alice)
	if resp.StatusCode != http.StatusNotFound || errCode(body) != "NAME_UNKNOWN" ||
		!strings.Contains(string(body), `blocked by routing rule \"edcom/*\" on repository \"docker-group\"`) {
		t.Errorf("all excluded tags = %d %s", resp.StatusCode, body)
	}
}

func TestRoutingProxyBlock(t *testing.T) {
	tr, remote, _, _ := routingReg(t)
	// Cache public/tool before any rule.
	resp, body := tr.do(http.MethodGet, "/v2/docker-proxy/public/tool/manifests/1", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	digest := resp.Header.Get("Docker-Content-Digest")

	setRouting(t, tr, "docker-proxy", &core.RoutingRules{Block: []string{"edcom/*", "public/*"}})
	for _, p := range []string{"edcom/api/manifests/9.9", "edcom/api/tags/list", "edcom/api/blobs/" + string(core.SHA256Of([]byte("x")))} {
		resp, body := tr.do(http.MethodGet, "/v2/docker-proxy/"+p, nil, alice)
		if resp.StatusCode != http.StatusNotFound || resp.Header.Get(routing.Header) == "" ||
			!strings.Contains(string(body), `blocked by routing rule \"edcom/*\" on repository \"docker-proxy\"`) {
			t.Errorf("GET %s = %d %s", p, resp.StatusCode, body)
		}
	}
	// Through the group the hosted image is served; the remote never hears the name.
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/edcom/api/manifests/1.0", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	resp, body = tr.do(http.MethodGet, "/v2/docker-group/edcom/api/manifests/9.9", nil, alice)
	tr.expect(resp, body, http.StatusNotFound)
	if remote.saw("edcom") {
		t.Error("an internal name reached the remote")
	}
	// Cached before the rule: not served, by tag or digest; not in the catalog.
	for _, ref := range []string{"1", digest} {
		resp, body := tr.do(http.MethodGet, "/v2/docker-proxy/public/tool/manifests/"+ref, nil, alice)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("cached manifest %s = %d %s", ref, resp.StatusCode, body)
		}
	}
	resp, body = tr.do(http.MethodGet, "/v2/_catalog", nil, alice)
	tr.expect(resp, body, http.StatusOK)
	if strings.Contains(string(body), "docker-proxy/public/tool") || strings.Contains(string(body), "docker-group/public/tool") ||
		!strings.Contains(string(body), "docker-hosted/edcom/api") {
		t.Errorf("catalog = %s", body)
	}
}

// A proxy of Docker Hub matches its rules against the name the remote sees:
// "library/*" covers the official images clients pull as "nginx".
func TestRoutingProxyDockerHubLibrary(t *testing.T) {
	tr := newTestReg(t)
	hub := &fakeHub{reg: registry.New(registry.Logger(log.New(io.Discard, "", 0)))}
	tr.env.AddRepo(t, &core.Repository{Name: "hub-proxy", Format: "docker", Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://registry-1.docker.io"},
			Routing: &core.RoutingRules{Block: []string{"library/*"}}}})
	tr.env.Upstreams.Set("hub-proxy", hub)
	resp, body := tr.do(http.MethodGet, "/v2/hub-proxy/nginx/manifests/latest", nil, alice)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), `\"library/*\"`) {
		t.Errorf("official image = %d %s", resp.StatusCode, body)
	}
	if n := tr.env.Upstreams.Calls("hub-proxy"); n != 0 {
		t.Errorf("remote contacted %d times", n)
	}
}
