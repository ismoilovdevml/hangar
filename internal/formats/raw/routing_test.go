package raw

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// routingFixture: raw-group of raw-hosted (internal/lib.txt) and raw-proxy,
// whose remote (a real HTTP server) also serves files under internal/.
func routingFixture(t *testing.T) (*fixture, *remote) {
	t.Helper()
	rm := newRemote(map[string]string{
		"/internal/tool.sh": "public impostor", "/internal/lib.txt": "public lib", "/public/a.txt": "public file",
	})
	srv := httptest.NewServer(rm)
	t.Cleanup(srv.Close)
	fx := newFixture(t,
		&core.Repository{Name: "raw-proxy", Format: Name, Type: core.RepoProxy,
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: srv.URL}}},
		&core.Repository{Name: "raw-group", Format: Name, Type: core.RepoGroup,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-hosted", "raw-proxy"}}}})
	if code := fx.put("raw-hosted/internal/lib.txt", "internal lib"); code != http.StatusCreated {
		t.Fatalf("put = %d", code)
	}
	return fx, rm
}

func setRouting(t *testing.T, fx *fixture, name string, rules *core.RoutingRules) {
	t.Helper()
	r := mustRepo(t, fx, name)
	r.Config.Routing = rules
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	fx.env.Repos.Put(r)
}

func (rm *remote) saw(sub string) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	for _, p := range rm.paths {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func TestRoutingGroup(t *testing.T) {
	fx, _ := routingFixture(t)
	// Unprotected: a file only the public remote has is served under the internal path.
	if code, _, body := fx.do(http.MethodGet, "raw-group/internal/tool.sh", nil, as("bob")); code != http.StatusOK || body != "public impostor" {
		t.Fatalf("unprotected = %d %q", code, body)
	}

	setRouting(t, fx, "raw-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "raw-hosted", Exclusive: []string{"internal/*"}}}})
	code, hdr, body := fx.do(http.MethodGet, "raw-group/internal/tool.sh", nil, as("bob"))
	if code != http.StatusNotFound || !strings.Contains(hdr.Get(routing.Header), `"internal/*"`) || !strings.Contains(body, "routing rule") {
		t.Errorf("protected = %d %q %s", code, hdr.Get(routing.Header), body)
	}
	if code, _, body := fx.do(http.MethodGet, "raw-group/internal/lib.txt", nil, as("bob")); code != http.StatusOK || body != "internal lib" {
		t.Errorf("hosted file = %d %q", code, body)
	}
	if code, _, body := fx.do(http.MethodGet, "raw-group/public/a.txt", nil, as("bob")); code != http.StatusOK || body != "public file" {
		t.Errorf("unclaimed path = %d %q", code, body)
	}
	// The proxy cached internal/tool.sh earlier; the group listing leaves it out.
	code, _, body = fx.do(http.MethodGet, "raw-group/internal/", nil, as("bob"), header("Accept", "application/json"))
	if code != http.StatusOK || !strings.Contains(body, "lib.txt") || strings.Contains(body, "tool.sh") {
		t.Errorf("group listing = %d %s", code, body)
	}
	// Every member excluded: 404 naming the rule.
	setRouting(t, fx, "raw-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "raw-hosted", Block: []string{"internal/*"}}, {Member: "raw-proxy", Block: []string{"internal/*"}}}})
	if code, _, body := fx.do(http.MethodGet, "raw-group/internal/lib.txt", nil, as("bob")); code != http.StatusNotFound ||
		!strings.Contains(body, `blocked by routing rule \"internal/*\" on repository \"raw-group\"`) {
		t.Errorf("all excluded = %d %s", code, body)
	}
}

func TestRoutingProxyBlock(t *testing.T) {
	fx, rm := routingFixture(t)
	// Cache a public file before any rule exists.
	if code, _, _ := fx.do(http.MethodGet, "raw-proxy/public/a.txt", nil, as("bob")); code != http.StatusOK {
		t.Fatalf("warm cache = %d", code)
	}
	setRouting(t, fx, "raw-proxy", &core.RoutingRules{Block: []string{"internal/*", "public/*"}})
	code, hdr, body := fx.do(http.MethodGet, "raw-proxy/internal/tool.sh", nil, as("bob"))
	if code != http.StatusNotFound || !strings.Contains(body, `blocked by routing rule \"internal/*\" on repository \"raw-proxy\"`) || hdr.Get(routing.Header) == "" {
		t.Errorf("proxy block = %d %s", code, body)
	}
	if code, _, body := fx.do(http.MethodGet, "raw-proxy/public/a.txt", nil, as("bob")); code != http.StatusNotFound {
		t.Errorf("cached before the rule = %d %s", code, body)
	}
	// Through the group the hosted copy is served and the remote never hears the path.
	if code, _, body := fx.do(http.MethodGet, "raw-group/internal/lib.txt", nil, as("bob")); code != http.StatusOK || body != "internal lib" {
		t.Errorf("group = %d %q", code, body)
	}
	if code, _, _ := fx.do(http.MethodGet, "raw-group/internal/other.txt", nil, as("bob")); code != http.StatusNotFound {
		t.Errorf("group miss = %d", code)
	}
	if rm.saw("internal") {
		t.Error("an internal path reached the remote")
	}
	if code, _, body := fx.do(http.MethodGet, "raw-proxy/", nil, as("bob"), header("Accept", "application/json")); code != http.StatusOK || strings.Contains(body, "a.txt") {
		t.Errorf("proxy listing = %d %s", code, body)
	}
}
