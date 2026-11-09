package gomod_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/gomod"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

const (
	internalMod = "git.example.com/corp/utils"
	publicMod   = "example.org/public/lib"
)

// publicGoProxy is the "public" remote of the routing tests: another Hangar
// serving a hosted Go repository over HTTP, where someone published a
// higher version of an internal module path. It logs request paths.
type publicGoProxy struct {
	srv *httptest.Server
	log *pathLog
}

func newPublicGoProxy(t *testing.T) *publicGoProxy {
	t.Helper()
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: "public", Format: gomod.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{AnonymousRead: true}})
	env.Auth.AddUser("pub", "pw")
	env.AuthZ.Grant("pub", "repo:*:write")
	p := &publicGoProxy{log: &pathLog{}}
	h := formats.NewHandler(env.Deps(), gomod.New())
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.log.add(r.URL.Path)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	for _, mv := range [][2]string{{internalMod, "v1.99.0"}, {publicMod, "v1.2.0"}} {
		req := httptest.NewRequest(http.MethodPut, "/repository/public/"+mv[0]+"/@v/"+mv[1]+".zip",
			bytes.NewReader(moduleZip(t, mv[0], mv[1], simpleModule(mv[0]))))
		req.SetBasicAuth("pub", "pw")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("publish %s upstream = %d %s", mv[0], rec.Code, rec.Body)
		}
	}
	p.log.reset()
	return p
}

func (l *pathLog) reset() { l.mu.Lock(); l.paths = nil; l.mu.Unlock() }

func (p *publicGoProxy) sawModule(mod string) bool {
	return p.log.any(func(path string) bool { return strings.Contains(path, mod) })
}

// routingFixture: go-hosted has the internal module at v1.0.0, go-proxy
// proxies the public remote (which has v1.99.0), go-group combines both.
func routingFixture(t *testing.T) (*fixture, *publicGoProxy) {
	t.Helper()
	pub := newPublicGoProxy(t)
	f := newFixture(t, func(env *coretest.Env, _ *core.Deps) {
		env.AddRepo(t, &core.Repository{Name: "go-proxy", Format: gomod.Name, Type: core.RepoProxy,
			Config: core.RepoConfig{AnonymousRead: true, Proxy: &core.ProxyConfig{RemoteURL: pub.srv.URL + "/repository/public"}}})
		env.AddRepo(t, &core.Repository{Name: "go-group", Format: gomod.Name, Type: core.RepoGroup,
			Config: core.RepoConfig{AnonymousRead: true, Group: &core.GroupConfig{Members: []string{"go-hosted", "go-proxy"}}}})
	})
	f.mustPublish(t, "go-hosted", internalMod, "v1.0.0", moduleZip(t, internalMod, "v1.0.0", simpleModule(internalMod)))
	return f, pub
}

func setRouting(t *testing.T, f *fixture, name string, rules *core.RoutingRules) {
	t.Helper()
	r, err := f.env.Repos.Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	r.Config.Routing = rules
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	f.env.Repos.Put(r)
}

func latestVersion(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var doc struct{ Version string }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &doc) != nil {
		t.Fatalf("@latest = %d %s", rec.Code, rec.Body)
	}
	return doc.Version
}

func TestRoutingGroupDependencyConfusion(t *testing.T) {
	f, _ := routingFixture(t)
	g := "/repository/go-group/"

	if rec := f.get(t, g+internalMod+"/@v/list"); rec.Body.String() != "v1.0.0\nv1.99.0\n" {
		t.Fatalf("unprotected list = %d %q (the test must bite)", rec.Code, rec.Body)
	}
	if v := latestVersion(t, f.get(t, g+internalMod+"/@latest")); v != "v1.99.0" {
		t.Fatalf("unprotected @latest = %s", v)
	}

	setRouting(t, f, "go-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "go-hosted", Exclusive: []string{"git.example.com/corp"}}}})
	rec := f.get(t, g+internalMod+"/@v/list")
	if rec.Code != http.StatusOK || rec.Body.String() != "v1.0.0\n" {
		t.Fatalf("protected list = %d %q", rec.Code, rec.Body)
	}
	if h := rec.Header().Get(routing.Header); !strings.Contains(h, `"git.example.com/corp"`) || !strings.Contains(h, `"go-proxy"`) {
		t.Errorf("%s = %q", routing.Header, h)
	}
	if v := latestVersion(t, f.get(t, g+internalMod+"/@latest")); v != "v1.0.0" {
		t.Errorf("protected @latest = %s", v)
	}
	for _, ext := range []string{".info", ".mod", ".zip"} {
		rec := f.get(t, g+internalMod+"/@v/v1.99.0"+ext)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "routing rule") {
			t.Errorf("public %s through group = %d %s", ext, rec.Code, rec.Body)
		}
	}
	if rec := f.get(t, g+internalMod+"/@v/v1.0.0.zip"); rec.Code != http.StatusOK {
		t.Errorf("hosted zip = %d", rec.Code)
	}
	// Unrelated public modules still come through the proxy member.
	if v := latestVersion(t, f.get(t, g+publicMod+"/@latest")); v != "v1.2.0" {
		t.Errorf("public module @latest = %s", v)
	}

	// Excluding every member answers 404 naming the rule.
	setRouting(t, f, "go-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "go-hosted", Block: []string{"git.example.com/corp"}}, {Member: "go-proxy", Block: []string{"git.example.com/corp"}}}})
	rec = f.get(t, g+internalMod+"/@v/list")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `blocked by routing rule "git.example.com/corp" on repository "go-group"`) {
		t.Errorf("all excluded = %d %s", rec.Code, rec.Body)
	}
}

func TestRoutingProxyBlockNeverReachesRemote(t *testing.T) {
	f, pub := routingFixture(t)
	p := "/repository/go-proxy/"
	// Cache the public zip of the internal path first: a later rule must hide it.
	if rec := f.get(t, p+internalMod+"/@v/v1.99.0.zip"); rec.Code != http.StatusOK {
		t.Fatalf("prime cache = %d %s", rec.Code, rec.Body)
	}
	pub.log.reset()

	setRouting(t, f, "go-proxy", &core.RoutingRules{Block: []string{"*.example.com/corp"}})
	for _, path := range []string{
		internalMod + "/@v/list",
		internalMod + "/@latest",
		internalMod + "/@v/v1.99.0.info",
		internalMod + "/@v/v1.99.0.zip", // cached before the rule
		internalMod + "/@v/main.info",   // a query
		"sumdb/sum.golang.org/lookup/" + internalMod + "@v1.99.0",
	} {
		rec := f.get(t, p+path)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `blocked by routing rule "*.example.com/corp" on repository "go-proxy"`) {
			t.Errorf("GET %s = %d %s", path, rec.Code, rec.Body)
		}
		if rec.Header().Get(routing.Header) == "" {
			t.Errorf("GET %s: no %s header", path, routing.Header)
		}
	}
	// Through the group the hosted version wins and the remote never hears the path.
	if rec := f.get(t, "/repository/go-group/"+internalMod+"/@v/list"); rec.Body.String() != "v1.0.0\n" {
		t.Errorf("group list = %q", rec.Body)
	}
	if rec := f.get(t, "/repository/go-group/sumdb/sum.golang.org/lookup/"+internalMod+"@v1.0.0"); rec.Code != http.StatusNotFound {
		t.Errorf("group sumdb lookup of the internal module = %d", rec.Code)
	}
	if pub.sawModule("corp/utils") {
		t.Error("the internal module path reached the public remote")
	}
	// Allowed modules still reach the remote.
	if v := latestVersion(t, f.get(t, p+publicMod+"/@latest")); v != "v1.2.0" {
		t.Errorf("public module = %s", v)
	}
	if !pub.sawModule(publicMod) {
		t.Error("allowed module never reached the remote")
	}
	// An allow list keeps everything else away from the remote.
	setRouting(t, f, "go-proxy", &core.RoutingRules{Allow: []string{"example.org"}})
	if rec := f.get(t, p+internalMod+"/@v/list"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "matches no allow pattern") {
		t.Errorf("allow miss = %d %s", rec.Code, rec.Body)
	}
}

// TestRealGoClientRouting resolves the internal module's @latest with the
// go command through the group: unprotected it picks the public v1.99.0,
// with an exclusive rule the hosted v1.0.0.
func TestRealGoClientRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("real-client test skipped in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	f, _ := routingFixture(t)
	srv := httptest.NewServer(f.h)
	t.Cleanup(srv.Close)
	f.env.BaseURL = srv.URL

	resolve := func(name string) string {
		t.Helper()
		work := filepath.Join(t.TempDir(), name)
		home := filepath.Join(work, "home")
		app := filepath.Join(work, "app")
		writeFiles(t, app, map[string]string{"go.mod": "module example.com/app\n\ngo 1.21\n"})
		writeFiles(t, home, map[string]string{".keep": ""})
		out := mustGo(t, work, home, app, []string{"GOPROXY=" + srv.URL + "/repository/go-group", "GOSUMDB=off", "GOFLAGS=-modcacherw -mod=mod"},
			"list", "-m", "-json", internalMod+"@latest")
		t.Logf("%s: go list -m -json %s@latest:\n%s", name, internalMod, out)
		var m struct{ Version string }
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("go list output: %v\n%s", err, out)
		}
		mustGo(t, work, home, app, []string{"GOPROXY=" + srv.URL + "/repository/go-group", "GOSUMDB=off", "GOFLAGS=-modcacherw -mod=mod"},
			"mod", "download", internalMod+"@"+m.Version)
		return m.Version
	}
	if v := resolve("unprotected"); v != "v1.99.0" {
		t.Fatalf("unprotected go list = %s, want the public v1.99.0 (the test must bite)", v)
	}
	setRouting(t, f, "go-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "go-hosted", Exclusive: []string{"git.example.com/corp"}}}})
	if v := resolve("protected"); v != "v1.0.0" {
		t.Fatalf("protected go list = %s, want the hosted v1.0.0", v)
	}
}
