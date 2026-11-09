package rpm_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm"
	"github.com/ismoilovdevml/hangar/internal/formats/rpm/rpmtest"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

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

func names(p *primary) string {
	var out []string
	for _, x := range p.Package {
		out = append(out, x.Name+"-"+x.Version.Ver)
	}
	return strings.Join(out, ",")
}

func TestRoutingGroup(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "rpm-other", Format: rpm.Name, Type: core.RepoHosted})
	f.env.AddRepo(t, &core.Repository{Name: "rpm-group", Format: rpm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"rpm-hosted", "rpm-other"}}}})
	internal := rpmtest.Package{Name: "edcom-tools", Version: "1.0"}
	impostor := rpmtest.Package{Name: "edcom-tools", Version: "99.0"}
	f.put(t, "rpm-hosted", internal.Filename(), internal)
	f.put(t, "rpm-other", impostor.Filename(), impostor)
	f.put(t, "rpm-other", "renamed.rpm", rpmtest.Package{Name: "edcom-extra", Version: "5"})
	f.put(t, "rpm-other", "other-tool-2-1.noarch.rpm", rpmtest.Package{Name: "other-tool", Version: "2"})

	prim, _ := repodata(t, f, "rpm-group", "")
	if got := names(prim); !strings.Contains(got, "edcom-tools-99.0") {
		t.Fatalf("unprotected primary = %s", got)
	}
	f.get(t, "/repository/rpm-group/"+impostor.Filename())

	setRouting(t, f, "rpm-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: "rpm-hosted", Exclusive: []string{"edcom-*"}}}})
	prim, _ = repodata(t, f, "rpm-group", "")
	if got := names(prim); got != "edcom-tools-1.0,other-tool-2" {
		t.Errorf("protected primary = %s", got)
	}
	for _, p := range []string{impostor.Filename(), "renamed.rpm"} {
		rec := f.do(t, http.MethodGet, "/repository/rpm-group/"+p, nil)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Header().Get(routing.Header), `"edcom-*"`) ||
			!strings.Contains(rec.Body.String(), `blocked by routing rule "edcom-*" on repository "rpm-group"`) {
			t.Errorf("GET %s = %d %q %s", p, rec.Code, rec.Header().Get(routing.Header), rec.Body)
		}
	}
	f.get(t, "/repository/rpm-group/"+internal.Filename())

	setRouting(t, f, "rpm-group", nil)
	if prim, _ := repodata(t, f, "rpm-group", ""); !strings.Contains(names(prim), "edcom-tools-99.0") {
		t.Error("repodata not regenerated after the rule was removed")
	}
}

// countingRemote serves the rpm-el hosted repository of the same server as
// a remote mirror and remembers the paths it was asked for.
type countingRemote struct {
	f     *fixture
	mu    sync.Mutex
	paths []string
}

func (c *countingRemote) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	c.mu.Lock()
	c.paths = append(c.paths, req.URL.Path)
	c.mu.Unlock()
	req2 := req.Clone(context.Background())
	req2.URL.Path = "/repository/rpm-el/" + strings.TrimPrefix(req.URL.Path, "/os/")
	req2.Header.Del("Authorization")
	c.f.h.ServeHTTP(w, req2)
}

func (c *countingRemote) saw(sub string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.paths {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func routingProxy(t *testing.T) (*fixture, *countingRemote) {
	t.Helper()
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "rpm-el", Format: rpm.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{AnonymousRead: true}})
	for _, p := range []rpmtest.Package{{Name: "edcom-tools", Version: "99.0"}, {Name: "public-lib", Version: "1"}} {
		f.put(t, "rpm-el", "Packages/"+p.Filename(), p)
	}
	rm := &countingRemote{f: f}
	f.env.Upstreams.Set("rpm-proxy", rm)
	f.env.AddRepo(t, &core.Repository{Name: "rpm-proxy", Format: rpm.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://mirror.example.org/os/", MetadataTTL: core.Duration(time.Hour)}}})
	return f, rm
}

func TestRoutingProxyBlock(t *testing.T) {
	f, rm := routingProxy(t)
	pub := "Packages/" + rpmtest.Package{Name: "public-lib", Version: "1"}.Filename()
	f.get(t, "/repository/rpm-proxy/"+pub) // cached before any rule
	setRouting(t, f, "rpm-proxy", &core.RoutingRules{Block: []string{"edcom-*", "public-*"}})

	// Signed metadata passes through unchanged.
	f.get(t, "/repository/rpm-proxy/repodata/repomd.xml")
	p := "Packages/" + rpmtest.Package{Name: "edcom-tools", Version: "99.0"}.Filename()
	rec := f.do(t, http.MethodGet, "/repository/rpm-proxy/"+p, nil)
	if rec.Code != http.StatusNotFound || rec.Header().Get(routing.Header) == "" ||
		!strings.Contains(rec.Body.String(), `blocked by routing rule "edcom-*" on repository "rpm-proxy"`) {
		t.Errorf("blocked package = %d %s", rec.Code, rec.Body)
	}
	if rm.saw("edcom") {
		t.Error("the blocked package was requested from the remote")
	}
	if rec := f.do(t, http.MethodGet, "/repository/rpm-proxy/"+pub, nil); rec.Code != http.StatusNotFound {
		t.Errorf("cached before the rule = %d", rec.Code)
	}
}
