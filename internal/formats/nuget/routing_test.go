package nuget_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// publicFeed is the "public" remote of the routing tests: another Hangar
// serving a hosted repository over real HTTP, where someone published a
// higher version of an internal package name. It counts the requests it gets
// and remembers their paths.
type publicFeed struct {
	*server
	hits  atomic.Int64
	paths chan string
}

func newPublicFeed(t *testing.T, pkgs ...[]byte) *publicFeed {
	t.Helper()
	p := &publicFeed{paths: make(chan string, 1024), server: &server{env: coretest.NewEnv(t)}}
	var h http.Handler
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.hits.Add(1)
		select {
		case p.paths <- r.URL.Path:
		default:
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(p.srv.Close)
	p.env.BaseURL = p.srv.URL
	h = formats.NewHandler(p.env.Deps(), nuget.New())
	p.env.AddRepo(t, &core.Repository{Name: "public", Format: nuget.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{AnonymousRead: true}})
	tok := p.env.Auth.AddToken("pub")
	p.env.AuthZ.Grant("pub", "repo:*:write")
	for _, pkg := range pkgs {
		pushTo(t, p.server, "public", tok, pkg)
	}
	p.hits.Store(0)
	for len(p.paths) > 0 {
		<-p.paths
	}
	return p
}

func (p *publicFeed) url() string { return p.srv.URL + "/repository/public" }

// sawPackage reports whether the feed was asked anything about id.
func (p *publicFeed) sawPackage(id string) bool {
	for {
		select {
		case path := <-p.paths:
			if strings.Contains(strings.ToLower(path), strings.ToLower(id)) {
				return true
			}
		default:
			return false
		}
	}
}

// newRoutingFixture builds nuget-hosted (Edcom.Common.Utils 1.0.0), a proxy
// of a public feed that also has Edcom.Common.Utils 99.0.0, and nuget-group
// over both.
func newRoutingFixture(t *testing.T) (*fixture, *publicFeed) {
	t.Helper()
	f := newFixture(t)
	feed := newPublicFeed(t, nupkg(t, "Edcom.Common.Utils", "99.0.0", ""), nupkg(t, "Newtonsoft.Json", "13.0.3", ""))
	f.env.AddRepo(t, &core.Repository{Name: "nuget-proxy", Format: nuget.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: feed.url(), MetadataTTL: core.Duration(time.Hour)},
			Format: map[string]any{"service_index": "index.json"}}})
	f.env.AddRepo(t, &core.Repository{Name: "nuget-group", Format: nuget.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"nuget-hosted", "nuget-proxy"}}}})
	f.mustPush("nuget-hosted", "Edcom.Common.Utils", "1.0.0", "")
	return f, feed
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

func flatVersions(t *testing.T, f *fixture, repo, lid string) []string {
	t.Helper()
	var flat struct {
		Versions []string `json:"versions"`
	}
	f.getJSON("/repository/"+repo+"/v3-flatcontainer/"+lid+"/index.json", &flat)
	return flat.Versions
}

func TestRoutingGroupDependencyConfusion(t *testing.T) {
	f, _ := newRoutingFixture(t)
	const lid = "edcom.common.utils"

	// Without rules the group merges the public 99.0.0: the attack works.
	if got := strings.Join(flatVersions(t, f, "nuget-group", lid), ","); got != "1.0.0,99.0.0" {
		t.Fatalf("unprotected versions = %s", got)
	}

	setRouting(t, f, "nuget-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "nuget-hosted", Exclusive: []string{"Edcom.*"}}}})

	rec := f.get("/repository/nuget-group/v3-flatcontainer/" + lid + "/index.json")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"1.0.0"`) || strings.Contains(rec.Body.String(), "99.0.0") {
		t.Fatalf("protected versions = %d %s", rec.Code, rec.Body)
	}
	if h := rec.Header().Get(routing.Header); !strings.Contains(h, `"Edcom.*"`) || !strings.Contains(h, `"nuget-proxy"`) {
		t.Errorf("%s = %q", routing.Header, h)
	}
	var reg regIndex
	f.getJSON("/repository/nuget-group/v3/registration-gz-semver2/"+lid+"/index.json", &reg)
	if got := strings.Join(reg.versions(), ","); got != "1.0.0" {
		t.Errorf("protected registration = %s", got)
	}
	// The public package cannot be downloaded through the group either.
	rec = f.get("/repository/nuget-group/v3-flatcontainer/" + lid + "/99.0.0/" + lid + ".99.0.0.nupkg")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "routing rule") {
		t.Errorf("public download through group = %d %s", rec.Code, rec.Body)
	}
	if rec := f.get("/repository/nuget-group/v3-flatcontainer/" + lid + "/1.0.0/" + lid + ".1.0.0.nupkg"); rec.Code != http.StatusOK {
		t.Errorf("hosted download = %d", rec.Code)
	}
	// Search through the group lists only the hosted package.
	var res searchResult
	f.getJSON("/repository/nuget-group/query?q=&semVerLevel=2.0.0", &res)
	for _, d := range res.Data {
		if strings.EqualFold(d.ID, "Edcom.Common.Utils") && d.Version != "1.0.0" {
			t.Errorf("search hit = %+v", d)
		}
	}
	if !strings.Contains(res.ids(), "Newtonsoft.Json@13.0.3") {
		t.Errorf("unrelated public package filtered from search: %s", res.ids())
	}
	// Other names still come from the proxy.
	if got := strings.Join(flatVersions(t, f, "nuget-group", "newtonsoft.json"), ","); got != "13.0.3" {
		t.Errorf("public package = %s", got)
	}
	// A rule excluding every member answers 404 naming the rule.
	setRouting(t, f, "nuget-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "nuget-hosted", Block: []string{"Edcom.*"}}, {Member: "nuget-proxy", Block: []string{"Edcom.*"}}}})
	rec = f.get("/repository/nuget-group/v3-flatcontainer/" + lid + "/index.json")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `blocked by routing rule "Edcom.*" on repository "nuget-group"`) {
		t.Errorf("all excluded = %d %s", rec.Code, rec.Body)
	}
}

func TestRoutingProxyBlockNeverReachesRemote(t *testing.T) {
	f, feed := newRoutingFixture(t)
	setRouting(t, f, "nuget-proxy", &core.RoutingRules{Block: []string{"Edcom.*"}})
	const lid = "edcom.common.utils"
	for _, p := range []string{
		"v3-flatcontainer/" + lid + "/index.json",
		"v3/registration-gz-semver2/" + lid + "/index.json",
		"v3-flatcontainer/" + lid + "/99.0.0/" + lid + ".99.0.0.nupkg",
	} {
		rec := f.get("/repository/nuget-proxy/" + p)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `blocked by routing rule "Edcom.*" on repository "nuget-proxy"`) {
			t.Errorf("GET %s = %d %s", p, rec.Code, rec.Body)
		}
		if rec.Header().Get(routing.Header) == "" {
			t.Errorf("GET %s: no %s header", p, routing.Header)
		}
	}
	// A search for the internal name is answered from the cache only.
	if rec := f.get("/repository/nuget-proxy/query?q=Edcom.Common.Utils"); rec.Code != http.StatusOK {
		t.Errorf("search = %d", rec.Code)
	}
	// Through the group: the hosted version, and the remote never hears the name.
	if got := strings.Join(flatVersions(t, f, "nuget-group", lid), ","); got != "1.0.0" {
		t.Errorf("group versions = %s", got)
	}
	if feed.sawPackage("edcom") {
		t.Error("the internal name reached the public remote")
	}
	// Other packages still pass.
	if got := strings.Join(flatVersions(t, f, "nuget-proxy", "newtonsoft.json"), ","); got != "13.0.3" {
		t.Errorf("public package = %s", got)
	}
	if feed.hits.Load() == 0 {
		t.Error("allowed package never reached the remote")
	}
	// A package cached before the rule was added is not served either.
	setRouting(t, f, "nuget-proxy", &core.RoutingRules{Block: []string{"Newtonsoft.*"}})
	if rec := f.get("/repository/nuget-proxy/v3-flatcontainer/newtonsoft.json/index.json"); rec.Code != http.StatusNotFound {
		t.Errorf("cached blocked package = %d", rec.Code)
	}
	// An allow list keeps everything else away from the remote.
	setRouting(t, f, "nuget-proxy", &core.RoutingRules{Allow: []string{"Newtonsoft.*"}})
	if rec := f.get("/repository/nuget-proxy/v3-flatcontainer/" + lid + "/index.json"); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "matches no allow pattern") {
		t.Errorf("allow miss = %d %s", rec.Code, rec.Body)
	}
}

// TestDotnetRouting restores a floating reference to an internal package
// through a group whose proxy member's remote has a higher public version:
// unprotected the client picks the public one, with a rule the hosted one.
func TestDotnetRouting(t *testing.T) {
	d := newDotnet(t)
	feed := newPublicFeed(t, nupkg(t, "Edcom.Common.Utils", "99.0.0", ""))
	s := newServer(t)
	s.env.AddRepo(t, &core.Repository{Name: "nuget-hosted", Format: nuget.Name, Type: core.RepoHosted})
	s.env.AddRepo(t, &core.Repository{Name: "nuget-proxy", Format: nuget.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: feed.url(), MetadataTTL: core.Duration(time.Hour)},
			Format: map[string]any{"service_index": "index.json"}}})
	group := s.env.AddRepo(t, &core.Repository{Name: "nuget-group", Format: nuget.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"nuget-hosted", "nuget-proxy"}}}})
	token := s.env.Auth.AddToken("ci")
	s.env.AuthZ.Grant("ci", "repo:*:read", "repo:*:write")
	pushTo(t, s, "nuget-hosted", token, nupkg(t, "Edcom.Common.Utils", "1.0.0", ""))

	restore := func(name string) string {
		app := filepath.Join(d.root, name)
		if err := os.MkdirAll(app, 0o755); err != nil {
			t.Fatal(err)
		}
		proj := `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net10.0</TargetFramework></PropertyGroup>
<ItemGroup><PackageReference Include="Edcom.Common.Utils" Version="*" /></ItemGroup></Project>`
		if err := os.WriteFile(filepath.Join(app, name+".csproj"), []byte(proj), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := filepath.Join(app, "nuget.config")
		writeConfig(t, cfg, s.source("nuget-group"), "ci", token)
		_ = os.RemoveAll(filepath.Join(d.root, "packages"))
		d.run(app, "restore", "--configfile", cfg, "--no-cache", "--force-evaluate")
		entries, _ := os.ReadDir(filepath.Join(d.root, "packages", "edcom.common.utils"))
		var vs []string
		for _, e := range entries {
			vs = append(vs, e.Name())
		}
		return strings.Join(vs, ",")
	}
	if got := restore("unprotected"); got != "99.0.0" {
		t.Fatalf("unprotected restore = %q, want the public 99.0.0 (the test must bite)", got)
	}
	group.Config.Routing = &core.RoutingRules{Members: []core.MemberRouting{{Member: "nuget-hosted", Exclusive: []string{"Edcom.*"}}}}
	s.env.Repos.Put(group)
	if got := restore("protected"); got != "1.0.0" {
		t.Fatalf("protected restore = %q, want the hosted 1.0.0", got)
	}
}

func pushTo(t *testing.T, s *server, repo, token string, pkg []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("package", "p.nupkg")
	_, _ = fw.Write(pkg)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPut, s.srv.URL+"/repository/"+repo+"/api/v2/package", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-NuGet-ApiKey", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("push = %d", resp.StatusCode)
	}
}
