package helm_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/helm"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// publicCharts is the "public" remote of the routing tests: a chart
// repository at /charts/ over real HTTP where someone published a higher
// version of an internal chart name, next to an unrelated public chart. It
// records every path asked.
type publicCharts struct {
	srv    *httptest.Server
	charts map[string][]byte // "name-version" -> archive
	mu     sync.Mutex
	paths  []string
}

func newPublicCharts(t *testing.T, charts map[string][]byte) *publicCharts {
	t.Helper()
	p := &publicCharts{charts: charts}
	p.srv = httptest.NewServer(p)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *publicCharts) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	if r.URL.Path == "/charts/index.yaml" {
		keys := make([]string, 0, len(p.charts))
		for k := range p.charts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("apiVersion: v1\nentries:\n")
		for _, k := range keys {
			i := strings.LastIndex(k, "-")
			name, vers := k[:i], k[i+1:]
			fmt.Fprintf(&b, "  %s:\n  - apiVersion: v2\n    name: %s\n    version: %s\n    created: \"2026-01-01T00:00:00Z\"\n    digest: %s\n    urls:\n    - %s.tgz\n",
				name, name, vers, sha(p.charts[k]), k)
		}
		b.WriteString("generated: \"2026-01-01T00:00:00Z\"\n")
		w.Header().Set("Content-Type", "application/x-yaml")
		_, _ = w.Write([]byte(b.String()))
		return
	}
	if k, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/charts/"), ".tgz"); ok {
		if b, ok := p.charts[k]; ok {
			_, _ = w.Write(b)
			return
		}
	}
	http.NotFound(w, r)
}

// askedFor reports whether a path other than index.yaml mentioned s.
func (p *publicCharts) askedFor(s string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, x := range p.paths {
		if x != "/charts/index.yaml" && strings.Contains(x, s) {
			return true
		}
	}
	return false
}

// newRoutingFixture: helm-hosted has edcom-api 1.0.0, helm-proxy's remote
// has edcom-api 99.0.0 and other-chart 1.0.0, helm-group has both.
func newRoutingFixture(t *testing.T) (*fixture, *publicCharts) {
	t.Helper()
	f := newFixture(t)
	pub := newPublicCharts(t, map[string][]byte{
		"edcom-api-99.0.0":  chartTgz(t, "edcom-api", "99.0.0", ""),
		"other-chart-1.0.0": chartTgz(t, "other-chart", "1.0.0", ""),
	})
	f.env.AddRepo(t, &core.Repository{Name: "helm-proxy", Format: helm.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: pub.srv.URL + "/charts"}}})
	f.env.AddRepo(t, &core.Repository{Name: "helm-group", Format: helm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{hostedName, "helm-proxy"}}}})
	f.mustUpload(hostedName, chartTgz(t, "edcom-api", "1.0.0", ""))
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

func chartVersions(idx index, name string) string {
	var vs []string
	for _, e := range idx.Entries[name] {
		vs = append(vs, e.Version)
	}
	return strings.Join(vs, ",")
}

func TestRoutingGroupDependencyConfusion(t *testing.T) {
	f, _ := newRoutingFixture(t)

	// Without rules the group merges the public 99.0.0: the attack works.
	if got := chartVersions(f.index("helm-group"), "edcom-api"); got != "99.0.0,1.0.0" {
		t.Fatalf("unprotected index = %s", got)
	}

	// The cached group index follows a rule change at once.
	setRouting(t, f, "helm-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: hostedName, Exclusive: []string{"edcom-*"}}}})
	rec := f.do(http.MethodGet, "/repository/helm-group/index.yaml", nil, "")
	var idx index
	if err := yaml.Unmarshal(rec.Body.Bytes(), &idx); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("index = %d %v", rec.Code, err)
	}
	if got := chartVersions(idx, "edcom-api"); got != "1.0.0" {
		t.Fatalf("protected index = %s", got)
	}
	if got := chartVersions(idx, "other-chart"); got != "1.0.0" {
		t.Errorf("unrelated public chart = %q", got)
	}
	if h := rec.Header().Get(routing.Header); !strings.Contains(h, `"edcom-*"`) || !strings.Contains(h, `"helm-proxy"`) {
		t.Errorf("%s = %q", routing.Header, h)
	}
	// The ChartMuseum read API agrees.
	if rec := f.do(http.MethodGet, "/repository/helm-group/api/charts/edcom-api/99.0.0", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("api version = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodGet, "/repository/helm-group/api/charts/edcom-api/latest", nil, ""); !strings.Contains(rec.Body.String(), `"version":"1.0.0"`) {
		t.Errorf("api latest = %d %s", rec.Code, rec.Body)
	}
	// The public archive cannot be downloaded through the group.
	for _, p := range []string{"edcom-api/99.0.0/edcom-api-99.0.0.tgz", "edcom-api-99.0.0.tgz"} {
		rec := f.do(http.MethodGet, "/repository/helm-group/"+p, nil, "")
		if rec.Code != http.StatusNotFound || rec.Header().Get(routing.Header) == "" || !strings.Contains(rec.Body.String(), "routing rule") {
			t.Errorf("public download %s through group = %d %s", p, rec.Code, rec.Body)
		}
	}
	if rec := f.do(http.MethodGet, "/repository/helm-group/edcom-api-1.0.0.tgz", nil, ""); rec.Code != http.StatusOK {
		t.Errorf("hosted download = %d", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/repository/helm-group/other-chart-1.0.0.tgz", nil, ""); rec.Code != http.StatusOK {
		t.Errorf("public download = %d %s", rec.Code, rec.Body)
	}

	// Removing the rule brings the public version back: nothing stale is cached.
	setRouting(t, f, "helm-group", nil)
	if got := chartVersions(f.index("helm-group"), "edcom-api"); got != "99.0.0,1.0.0" {
		t.Errorf("rule removed, index = %s", got)
	}

	// A rule excluding every member answers 404 naming the rule.
	setRouting(t, f, "helm-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: hostedName, Block: []string{"edcom-*"}}, {Member: "helm-proxy", Block: []string{"edcom-*"}}}})
	rec = f.do(http.MethodGet, "/repository/helm-group/edcom-api-1.0.0.tgz", nil, "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `blocked by routing rule \"edcom-*\" on repository \"helm-group\"`) {
		t.Errorf("all excluded = %d %s", rec.Code, rec.Body)
	}
	if _, ok := f.index("helm-group").Entries["edcom-api"]; ok {
		t.Error("blocked chart listed in the group index")
	}
}

func TestRoutingProxyBlockNeverReachesRemote(t *testing.T) {
	f, pub := newRoutingFixture(t)
	setRouting(t, f, "helm-proxy", &core.RoutingRules{Block: []string{"edcom-*"}})
	for _, p := range []string{"edcom-api/99.0.0/edcom-api-99.0.0.tgz", "edcom-api-99.0.0.tgz", "edcom-api/99.0.0/edcom-api-99.0.0.tgz.prov"} {
		rec := f.do(http.MethodGet, "/repository/helm-proxy/"+p, nil, "")
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `blocked by routing rule \"edcom-*\" on repository \"helm-proxy\"`) {
			t.Errorf("GET %s = %d %s", p, rec.Code, rec.Body)
		}
		if rec.Header().Get(routing.Header) == "" {
			t.Errorf("GET %s: no %s header", p, routing.Header)
		}
	}
	// The proxy's own index leaves the chart out; the group serves the hosted version.
	rec := f.do(http.MethodGet, "/repository/helm-proxy/index.yaml", nil, "")
	if strings.Contains(rec.Body.String(), "edcom-api") || !strings.Contains(rec.Body.String(), "other-chart") {
		t.Errorf("proxy index:\n%s", rec.Body)
	}
	if rec.Header().Get(routing.Header) == "" {
		t.Errorf("proxy index: no %s header", routing.Header)
	}
	if got := chartVersions(f.index("helm-group"), "edcom-api"); got != "1.0.0" {
		t.Errorf("group index = %s", got)
	}
	if rec := f.do(http.MethodGet, "/repository/helm-group/edcom-api-99.0.0.tgz", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("group public download = %d", rec.Code)
	}
	if pub.askedFor("edcom") {
		t.Error("an archive of the internal chart was requested from the public remote")
	}
	// Allowed charts still pass.
	if rec := f.do(http.MethodGet, "/repository/helm-proxy/other-chart-1.0.0.tgz", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("public download = %d %s", rec.Code, rec.Body)
	}
	if !pub.askedFor("other-chart") {
		t.Error("allowed chart never reached the remote")
	}
	// A chart cached before the rule is not served after it.
	setRouting(t, f, "helm-proxy", &core.RoutingRules{Block: []string{"other-*"}})
	if rec := f.do(http.MethodGet, "/repository/helm-proxy/other-chart-1.0.0.tgz", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("cached blocked chart = %d", rec.Code)
	}
	if _, ok := f.index("helm-proxy").Entries["other-chart"]; ok {
		t.Error("cached blocked chart listed")
	}
	// An allow list keeps everything else away.
	setRouting(t, f, "helm-proxy", &core.RoutingRules{Allow: []string{"other-*"}})
	idx := f.index("helm-proxy")
	if _, ok := idx.Entries["edcom-api"]; ok || len(idx.Entries["other-chart"]) != 1 {
		t.Errorf("allow list index = %+v", idx.Entries)
	}
	rec = f.do(http.MethodGet, "/repository/helm-proxy/edcom-api-99.0.0.tgz", nil, "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "matches no allow pattern") {
		t.Errorf("allow miss = %d %s", rec.Code, rec.Body)
	}
}

// TestHelmClientRouting pulls edcom-api with the real helm through a group
// whose proxy member's remote has a higher public version: unprotected
// helm picks the public one, with a rule the hosted one.
func TestHelmClientRouting(t *testing.T) {
	c := newHelm(t)
	f, srv := clientServer(t)
	pub := newPublicCharts(t, map[string][]byte{"edcom-api-99.0.0": readFile(t, c.pkg("edcom-api", "99.0.0"))})
	f.env.AddRepo(t, &core.Repository{Name: "helm-proxy", Format: helm.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: pub.srv.URL + "/charts"}}})
	f.env.AddRepo(t, &core.Repository{Name: "helm-group", Format: helm.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{hostedName, "helm-proxy"}}}})
	f.uploadFile(srv, http.MethodPost, "/repository/"+hostedName+"/api/charts", readFile(t, c.pkg("edcom-api", "1.0.0")), "")

	c.run("repo", "add", "corp", srv.URL+"/repository/helm-group", "--username", "alice", "--password", f.token)
	pull := func(dir string) string {
		c.run("repo", "update")
		out := c.run("search", "repo", "corp/edcom-api")
		d := c.mkdir(dir)
		c.run("pull", "corp/edcom-api", "-d", d)
		entries, _ := os.ReadDir(d)
		if len(entries) != 1 {
			t.Fatalf("pulled %d files\n%s", len(entries), out)
		}
		if !strings.Contains(out, strings.TrimSuffix(strings.TrimPrefix(entries[0].Name(), "edcom-api-"), ".tgz")) {
			t.Errorf("search and pull disagree:\n%s\n%s", out, entries[0].Name())
		}
		return filepath.Base(entries[0].Name())
	}
	if got := pull("unprotected"); got != "edcom-api-99.0.0.tgz" {
		t.Fatalf("unprotected pull = %s, want the public 99.0.0 (the test must bite)", got)
	}
	setRouting(t, f, "helm-group", &core.RoutingRules{Members: []core.MemberRouting{
		{Member: hostedName, Exclusive: []string{"edcom-*"}}}})
	if got := pull("protected"); got != "edcom-api-1.0.0.tgz" {
		t.Fatalf("protected pull = %s, want the hosted 1.0.0", got)
	}
}
