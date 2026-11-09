package npm_test

import (
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func searchNames(t *testing.T, ts *testServer, repo, query string) ([]string, map[string]any) {
	t.Helper()
	resp := ts.do(t, http.MethodGet, "/repository/"+repo+"/-/v1/search?"+query, "alice", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search %s %s = %d %s", repo, query, resp.StatusCode, readAll(t, resp))
	}
	doc := decodeJSON(t, resp)
	var names []string
	for _, o := range doc["objects"].([]any) {
		names = append(names, o.(map[string]any)["package"].(map[string]any)["name"].(string))
	}
	return names, doc
}

// Regression: npm search answered 404 on every repository type. Hosted
// repositories search their components, proxies ask their remote, groups
// merge their members (first member wins, routing rules apply).
func TestSearch(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-hosted", "@acme/widget-kit", "1.0.0", "latest")
	ts.publish(t, "npm-hosted", "@acme/widget-kit", "1.2.0", "latest")
	ts.publish(t, "npm-hosted", "other-thing", "0.1.0", "latest")
	ts.publish(t, "npm-upstream", "widget-core", "3.0.0", "latest")
	ts.publish(t, "npm-upstream", "@acme/widget-kit", "9.9.9", "latest") // shadowed by the hosted member

	names, doc := searchNames(t, ts, "npm-hosted", "text=widget")
	if !slices.Equal(names, []string{"@acme/widget-kit"}) {
		t.Fatalf("hosted search = %v", names)
	}
	pkg := doc["objects"].([]any)[0].(map[string]any)["package"].(map[string]any)
	if pkg["version"] != "1.2.0" || pkg["description"] != "test package" || pkg["scope"] != "acme" ||
		pkg["publisher"].(map[string]any)["username"] != "alice" {
		t.Fatalf("hosted package = %v", pkg)
	}

	if names, _ := searchNames(t, ts, "npm-proxy", "text=widget"); !slices.Contains(names, "widget-core") {
		t.Fatalf("proxy search = %v", names)
	}

	names, doc = searchNames(t, ts, "npm-group", "text=widget")
	if !slices.Contains(names, "widget-core") || !slices.Contains(names, "@acme/widget-kit") || len(names) != 2 {
		t.Fatalf("group search = %v", names)
	}
	for _, o := range doc["objects"].([]any) {
		p := o.(map[string]any)["package"].(map[string]any)
		if p["name"] == "@acme/widget-kit" && p["version"] != "1.2.0" {
			t.Fatalf("group: first member must win, got %v", p)
		}
	}
	if names, _ := searchNames(t, ts, "npm-group", "text=widget&size=1&from=1"); len(names) != 1 {
		t.Fatalf("paged group search = %v", names)
	}

	// A proxy rule keeps the remote's @acme results out.
	ts.env.AddRepo(t, &core.Repository{Name: "npm-ruled", Format: "npm", Type: core.RepoProxy, Config: core.RepoConfig{
		Proxy:   &core.ProxyConfig{RemoteURL: ts.url + "/repository/npm-upstream"},
		Routing: &core.RoutingRules{Block: []string{"@acme/*"}}}})
	if names, _ := searchNames(t, ts, "npm-ruled", "text=widget"); slices.Contains(names, "@acme/widget-kit") || !slices.Contains(names, "widget-core") {
		t.Fatalf("routing-filtered proxy search = %v", names)
	}

	if r := ts.do(t, http.MethodGet, "/repository/npm-hosted/-/v1/search?text=x&size=nope", "alice", nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad size = %d", r.StatusCode)
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-hosted/-/v1/search?text=widget", "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous search = %d", r.StatusCode)
	}
}

// TestRealNpmSearch: `npm search` prints Hangar's results.
func TestRealNpmSearch(t *testing.T) {
	npmBin, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not installed")
	}
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	ts := newServer(t)
	ts.publish(t, "npm-hosted", "@acme/widget-kit", "1.2.0", "latest")
	ts.publish(t, "npm-upstream", "widget-core", "3.0.0", "latest")
	c := newClient(t, ts, ts.env.Auth.AddToken("alice"))
	out := c.must(t, t.TempDir(), npmBin, "search", "widget", "--registry", ts.registry("npm-group"))
	if !strings.Contains(out, "@acme/widget-kit") || !strings.Contains(out, "widget-core") || !strings.Contains(out, "1.2.0") {
		t.Fatalf("npm search = %q", out)
	}
}
