package npm_test

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// setRouting replaces a repository's routing rules.
func (ts *testServer) setRouting(t *testing.T, name string, rules *core.RoutingRules) {
	t.Helper()
	r, err := ts.env.Repos.Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	r.Config.Routing = rules
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	ts.env.Repos.Put(r)
}

func versionKeys(doc map[string]any) string {
	var vs []string
	for v := range doc["versions"].(map[string]any) {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return strings.Join(vs, ",")
}

var exclusiveTrk = &core.RoutingRules{Members: []core.MemberRouting{{Member: "npm-hosted", Exclusive: []string{"@trk/*"}}}}

// The proxy's remote (npm-upstream) has a higher public version of the
// internal @trk/utils: without rules the group lists it, with an exclusive
// rule on the hosted member it does not.
func TestRoutingGroupDependencyConfusion(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-hosted", "@trk/utils", "1.0.0", "latest")
	ts.publish(t, "npm-upstream", "@trk/utils", "99.0.0", "latest")
	ts.publish(t, "npm-upstream", "left-pad", "1.3.0", "latest")

	doc := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-group/@trk%2futils", "alice", nil))
	if got := versionKeys(doc); got != "1.0.0,99.0.0" {
		t.Fatalf("unprotected versions = %s (the test must bite)", got)
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-group/@trk/utils/-/utils-99.0.0.tgz", "alice", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("unprotected public tarball = %d", r.StatusCode)
	}

	ts.setRouting(t, "npm-group", exclusiveTrk)
	resp := ts.do(t, http.MethodGet, "/repository/npm-group/@trk%2futils", "alice", nil)
	if h := resp.Header.Get(routing.Header); !strings.Contains(h, `"@trk/*"`) || !strings.Contains(h, `"npm-proxy"`) {
		t.Errorf("%s = %q", routing.Header, h)
	}
	doc = decodeJSON(t, resp)
	if got := versionKeys(doc); got != "1.0.0" {
		t.Fatalf("protected versions = %s", got)
	}
	if doc["dist-tags"].(map[string]any)["latest"] != "1.0.0" {
		t.Errorf("latest = %v", doc["dist-tags"])
	}
	abbr := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-group/@trk%2futils", "alice", nil,
		"Accept", "application/vnd.npm.install-v1+json"))
	if got := versionKeys(abbr); got != "1.0.0" {
		t.Errorf("abbreviated versions = %s", got)
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-group/@trk%2futils/99.0.0", "alice", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("version document = %d", r.StatusCode)
	}
	tags := decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-group/-/package/@trk%2futils/dist-tags", "alice", nil))
	if tags["latest"] != "1.0.0" {
		t.Errorf("dist-tags = %v", tags)
	}
	// The public tarball, even though the proxy cached it, is not served through the group.
	r := ts.do(t, http.MethodGet, "/repository/npm-group/@trk/utils/-/utils-99.0.0.tgz", "alice", nil)
	if body := string(readAll(t, r)); r.StatusCode != http.StatusNotFound || !strings.Contains(body, "routing rule") {
		t.Errorf("public tarball through group = %d %s", r.StatusCode, body)
	}
	if r.Header.Get(routing.Header) == "" {
		t.Errorf("tarball 404 without %s", routing.Header)
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-group/@trk/utils/-/utils-1.0.0.tgz", "alice", nil); r.StatusCode != http.StatusOK {
		t.Errorf("hosted tarball = %d", r.StatusCode)
	}
	// Other names still come from the proxy.
	if got := versionKeys(decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-group/left-pad", "alice", nil))); got != "1.3.0" {
		t.Errorf("public package = %s", got)
	}
	// Every member excluded: a 404 naming the rule.
	ts.setRouting(t, "npm-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "npm-hosted", Block: []string{"@trk/*"}}, {Member: "npm-proxy", Block: []string{"@trk/*"}}}})
	r = ts.do(t, http.MethodGet, "/repository/npm-group/@trk%2futils", "alice", nil)
	if body := string(readAll(t, r)); r.StatusCode != http.StatusNotFound ||
		!strings.Contains(body, `blocked by routing rule \"@trk/*\" on repository \"npm-group\"`) {
		t.Errorf("all excluded = %d %s", r.StatusCode, body)
	}
}

func TestRoutingProxyBlockNeverReachesRemote(t *testing.T) {
	ts := newServer(t)
	ts.publish(t, "npm-hosted", "@trk/utils", "1.0.0", "latest")
	ts.publish(t, "npm-upstream", "@trk/utils", "99.0.0", "latest")
	ts.publish(t, "npm-upstream", "left-pad", "1.3.0", "latest")
	ts.setRouting(t, "npm-proxy", &core.RoutingRules{Block: []string{"@trk/*"}})

	for _, p := range []string{"@trk%2futils", "@trk%2futils/99.0.0", "@trk/utils/-/utils-99.0.0.tgz", "-/package/@trk%2futils/dist-tags"} {
		r := ts.do(t, http.MethodGet, "/repository/npm-proxy/"+p, "alice", nil)
		body := string(readAll(t, r))
		if r.StatusCode != http.StatusNotFound || !strings.Contains(body, `blocked by routing rule \"@trk/*\" on repository \"npm-proxy\"`) {
			t.Errorf("GET %s = %d %s", p, r.StatusCode, body)
		}
		if r.Header.Get(routing.Header) == "" {
			t.Errorf("GET %s: no %s header", p, routing.Header)
		}
	}
	// Through the group: the hosted version only.
	if got := versionKeys(decodeJSON(t, ts.do(t, http.MethodGet, "/repository/npm-group/@trk%2futils", "alice", nil))); got != "1.0.0" {
		t.Errorf("group versions = %s", got)
	}
	if n := ts.env.Upstreams.Calls("npm-proxy"); n != 0 {
		t.Fatalf("the remote was asked %d times about a blocked name", n)
	}
	// Allowed names still reach the remote.
	if r := ts.do(t, http.MethodGet, "/repository/npm-proxy/left-pad", "alice", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("allowed package = %d", r.StatusCode)
	}
	if r := ts.do(t, http.MethodGet, "/repository/npm-proxy/left-pad/-/left-pad-1.3.0.tgz", "alice", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("allowed tarball = %d", r.StatusCode)
	}
	if ts.env.Upstreams.Calls("npm-proxy") == 0 {
		t.Fatal("allowed package never reached the remote")
	}
	// A package cached before the rule existed is not served after it.
	ts.setRouting(t, "npm-proxy", &core.RoutingRules{Block: []string{"left-*"}})
	before := ts.env.Upstreams.Calls("npm-proxy")
	for _, p := range []string{"left-pad", "left-pad/-/left-pad-1.3.0.tgz"} {
		if r := ts.do(t, http.MethodGet, "/repository/npm-proxy/"+p, "alice", nil); r.StatusCode != http.StatusNotFound {
			t.Errorf("cached blocked %s = %d", p, r.StatusCode)
		}
	}
	if ts.env.Upstreams.Calls("npm-proxy") != before {
		t.Error("blocked cached package reached the remote")
	}
	// An allow list keeps every other name away from the remote.
	ts.setRouting(t, "npm-proxy", &core.RoutingRules{Allow: []string{"left-*"}})
	r := ts.do(t, http.MethodGet, "/repository/npm-proxy/@trk%2futils", "alice", nil)
	if body := string(readAll(t, r)); r.StatusCode != http.StatusNotFound || !strings.Contains(body, "matches no allow pattern") {
		t.Errorf("allow miss = %d %s", r.StatusCode, body)
	}
}

// TestNpmClientRouting installs a "*" range of an internal package through
// a group that lists the proxy first (so its "latest" tag wins): unprotected
// npm and pnpm pick the public 99.0.0, with a rule the hosted 1.0.0.
func TestNpmClientRouting(t *testing.T) {
	npmBin, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not installed")
	}
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	ts := newServer(t)
	c := newClient(t, ts, ts.env.Auth.AddToken("alice"))
	group := ts.registry("npm-group")
	g, _ := ts.env.Repos.Get(t.Context(), "npm-group")
	g.Config.Group.Members = []string{"npm-proxy", "npm-hosted"}
	ts.env.Repos.Put(g)
	c.must(t, writePackage(t, "@trk/utils", "1.0.0", nil), npmBin, "publish", "--registry", ts.registry("npm-hosted"))
	c.must(t, writePackage(t, "@trk/utils", "99.0.0", nil), npmBin, "publish", "--registry", ts.registry("npm-upstream"))

	install := func() string {
		dir := writeProject(t, map[string]string{"@trk/utils": "*"})
		c.must(t, dir, npmBin, "install", "--registry", group, "--cache", t.TempDir(), "--prefer-online")
		return filepath.Join(dir, "node_modules/@trk/utils/package.json")
	}
	// Recent pnpm releases skip versions younger than minimum-release-age;
	// both versions here are seconds old, so the age check is turned off to
	// show what the registry itself resolves to.
	pnpmBin, pnpmErr := exec.LookPath("pnpm")
	pnpmInstall := func() string {
		dir := writeProject(t, map[string]string{"@trk/utils": "*"})
		c.must(t, dir, pnpmBin, "install", "--registry", group, "--store-dir", t.TempDir(), "--prefer-offline=false", "--config.minimum-release-age=0")
		return filepath.Join(dir, "node_modules/@trk/utils/package.json")
	}
	fileContains(t, install(), `"version": "99.0.0"`)
	if pnpmErr == nil {
		fileContains(t, pnpmInstall(), `"version": "99.0.0"`)
	}

	ts.setRouting(t, "npm-group", exclusiveTrk)
	fileContains(t, install(), `"version": "1.0.0"`)
	if pnpmErr == nil {
		fileContains(t, pnpmInstall(), `"version": "1.0.0"`)
	} else {
		t.Log("pnpm not installed: pnpm part skipped")
	}
}
