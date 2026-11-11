package nuget_test

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
)

// fakeFeed imitates nuget.org's layout: gzip registrations with linked
// pages and a search service (on the feed host, or on another host when
// offHost is set, as on nuget.org).
type fakeFeed struct {
	t       *testing.T
	pkg     []byte
	search  *httptest.Server
	down    atomic.Bool
	offHost atomic.Bool
}

const remote = "https://upstream.test"

func newFakeFeed(t *testing.T) *fakeFeed {
	f := &fakeFeed{t: t, pkg: nupkg(t, "Remote.Pkg", "1.0.0", "")}
	f.search = httptest.NewServer(http.HandlerFunc(f.serveSearch))
	t.Cleanup(f.search.Close)
	return f
}

func (f *fakeFeed) serveSearch(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{"totalHits": 1, "data": []any{map[string]any{
		"@id": remote + "/v3/reg-gz/remote.pkg/index.json", "registration": remote + "/v3/reg-gz/remote.pkg/index.json",
		"id": "Remote.Pkg", "version": "1.0.0", "q": r.URL.Query().Get("q"),
		"versions": []any{map[string]any{"version": "1.0.0", "downloads": 5, "@id": remote + "/v3/reg-gz/remote.pkg/1.0.0.json"}},
	}}})
}

func catalog(version string, listed bool) map[string]any {
	return map[string]any{
		"@id": "https://upstream.test/v3/catalog0/data/remote.pkg." + version + ".json", "id": "Remote.Pkg", "version": version,
		"listed": listed, "description": "from upstream", "packageContent": remote + "/v3-flatcontainer/remote.pkg/" + version + "/remote.pkg." + version + ".nupkg",
		"dependencyGroups": []any{map[string]any{"targetFramework": "net10.0", "dependencies": []any{
			map[string]any{"id": "Other", "range": "[1.0.0, )", "registration": remote + "/v3/reg-gz/other/index.json"},
		}}},
	}
}

func (f *fakeFeed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		http.Error(w, "down", http.StatusBadGateway)
		return
	}
	writeGz := func(v any) {
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_ = json.NewEncoder(zw).Encode(v)
		_ = zw.Close()
	}
	switch r.URL.Path {
	case "/v3/index.json":
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "3.0.0", "resources": []any{
			map[string]any{"@id": remote + "/v3-flatcontainer/", "@type": "PackageBaseAddress/3.0.0"},
			map[string]any{"@id": remote + "/v3/reg/", "@type": "RegistrationsBaseUrl"},
			map[string]any{"@id": remote + "/v3/reg-gz/", "@type": "RegistrationsBaseUrl/3.6.0"},
			map[string]any{"@id": f.searchURL(), "@type": "SearchQueryService/3.0.0-rc"},
		}})
	case "/v3/reg-gz/remote.pkg/index.json":
		writeGz(map[string]any{"count": 1, "items": []any{map[string]any{
			"@id": remote + "/v3/reg-gz/remote.pkg/page1.json", "count": 2, "lower": "1.0.0", "upper": "2.0.0",
		}}})
	case "/v3/reg-gz/remote.pkg/page1.json":
		writeGz(map[string]any{"count": 2, "items": []any{
			map[string]any{"catalogEntry": catalog("1.0.0", true)},
			map[string]any{"catalogEntry": catalog("2.0.0", false)},
		}})
	case "/query":
		f.serveSearch(w, r)
	case "/v3-flatcontainer/remote.pkg/index.json":
		_ = json.NewEncoder(w).Encode(map[string]any{"versions": []string{"1.0.0", "2.0.0"}})
	case "/v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg":
		_, _ = w.Write(f.pkg)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeFeed) searchURL() string {
	if f.offHost.Load() {
		return f.search.URL + "/query"
	}
	return remote + "/query"
}

func newProxyFixture(t *testing.T) (*fixture, *fakeFeed) {
	t.Helper()
	f := newFixture(t)
	feed := newFakeFeed(t)
	for name, ttl := range map[string]time.Duration{"nuget-proxy": time.Hour, "nuget-proxy-0": -1} {
		f.env.AddRepo(t, &core.Repository{Name: name, Format: nuget.Name, Type: core.RepoProxy,
			Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: remote, MetadataTTL: core.Duration(ttl)}}})
		f.env.Upstreams.Set(name, feed)
	}
	f.env.AddRepo(t, &core.Repository{Name: "nuget-group", Format: nuget.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"nuget-hosted", "nuget-proxy"}}}})
	f.h = formats.NewHandler(f.env.Deps(), nuget.New())
	return f, feed
}

func TestProxy(t *testing.T) {
	f, feed := newProxyFixture(t)

	rec := f.get("/repository/nuget-proxy/v3/registration-gz-semver2/remote.pkg/index.json")
	if rec.Code != 200 {
		t.Fatalf("registration = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "upstream.test") {
		t.Errorf("upstream URLs leaked:\n%s", rec.Body)
	}
	var reg regIndex
	_ = json.Unmarshal(rec.Body.Bytes(), &reg)
	if got := strings.Join(reg.versions(), ","); got != "1.0.0,2.0.0" {
		t.Errorf("versions = %s", got)
	}
	leaf := reg.Items[0].Items[0]
	if leaf.PackageContent != "http://hangar.test/repository/nuget-proxy/v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg" ||
		reg.Items[0].Items[1].CatalogEntry.Listed ||
		leaf.CatalogEntry.DependencyGroups[0].Dependencies[0].Registration != "http://hangar.test/repository/nuget-proxy/v3/registration-gz-semver2/other/index.json" {
		t.Errorf("rewritten leaf = %+v", leaf)
	}

	// Metadata is cached for the TTL.
	calls := f.env.Upstreams.Calls("nuget-proxy")
	f.get("/repository/nuget-proxy/v3/registration/remote.pkg/index.json")
	if n := f.env.Upstreams.Calls("nuget-proxy"); n != calls {
		t.Errorf("registration refetched within TTL: %d -> %d calls", calls, n)
	}

	var flat struct {
		Versions []string `json:"versions"`
	}
	f.getJSON("/repository/nuget-proxy/v3-flatcontainer/remote.pkg/index.json", &flat)
	if strings.Join(flat.Versions, ",") != "1.0.0,2.0.0" {
		t.Errorf("flat = %v", flat.Versions)
	}

	// Packages are fetched once and then served from the cache, by a reader.
	for range 2 {
		req := httptest.NewRequest(http.MethodGet, "/repository/nuget-proxy/v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg", nil)
		req.SetBasicAuth("reader", "pw")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.Len() != len(feed.pkg) {
			t.Fatalf("proxied download = %d %s", rec.Code, rec.Body)
		}
	}
	calls = f.env.Upstreams.Calls("nuget-proxy")
	f.get("/repository/nuget-proxy/v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.nuspec")
	if n := f.env.Upstreams.Calls("nuget-proxy"); n != calls {
		t.Errorf("cached package refetched: %d -> %d", calls, n)
	}
	repo, _ := f.env.Repos.Get(t.Context(), "nuget-proxy")
	if c, err := f.env.Components.GetComponent(t.Context(), repo.ID, "", "remote.pkg", "1.0.0"); err != nil || c.Attributes["id"] != "Remote.Pkg" {
		t.Errorf("cached component = %+v, %v", c, err)
	}
	if rec := f.get("/repository/nuget-proxy/v3-flatcontainer/remote.pkg/2.0.0/remote.pkg.2.0.0.nupkg"); rec.Code != 404 {
		t.Errorf("missing upstream package = %d", rec.Code)
	}
	if rec := f.get("/repository/nuget-proxy/v3/registration/nothing/index.json"); rec.Code != 404 {
		t.Errorf("missing upstream registration = %d", rec.Code)
	}

	// Search is forwarded to the upstream search service; results point back at Hangar.
	rec = f.get("/repository/nuget-proxy/query?q=remote&take=5")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "upstream.test") || !strings.Contains(rec.Body.String(), `"q":"remote"`) {
		t.Errorf("proxied search = %d %s", rec.Code, rec.Body)
	}
	// A search service on another host (nuget.org's azuresearch-*) is a
	// related host declared by the service index: it is followed through the
	// repository's upstream. When it fails, the proxy searches its cache.
	feed.offHost.Store(true)
	f.env.AddRepo(t, &core.Repository{Name: "nuget-proxy-offhost", Format: nuget.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: remote}}})
	f.env.Upstreams.Set("nuget-proxy-offhost", feed)
	f.get("/repository/nuget-proxy-offhost/v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg")
	rec = f.get("/repository/nuget-proxy-offhost/query?q=remote")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"q":"remote"`) || strings.Contains(rec.Body.String(), "upstream.test") {
		t.Errorf("off-host search = %d %s", rec.Code, rec.Body)
	}
	f.env.Upstreams.Set("nuget-proxy-offhost", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/query" {
			http.Error(w, "search down", http.StatusServiceUnavailable)
			return
		}
		feed.ServeHTTP(w, r)
	}))
	var sr searchResult
	f.getJSON("/repository/nuget-proxy-offhost/query?q=remote", &sr)
	if sr.ids() != "Remote.Pkg@1.0.0" || strings.Contains(fmt.Sprint(sr), "upstream.test") || strings.Contains(fmt.Sprint(sr), "q:remote") {
		t.Errorf("fallback search of the cache = %+v", sr)
	}
	feed.offHost.Store(false)

	// With a negative TTL every request revalidates; a failing upstream serves stale metadata.
	f.getJSON("/repository/nuget-proxy-0/v3-flatcontainer/remote.pkg/index.json", &flat)
	feed.down.Store(true)
	flat.Versions = nil
	f.getJSON("/repository/nuget-proxy-0/v3-flatcontainer/remote.pkg/index.json", &flat)
	if len(flat.Versions) != 2 {
		t.Errorf("stale flat = %v", flat.Versions)
	}
	if rec := f.get("/repository/nuget-proxy-0/v3/registration/remote.pkg/index.json"); rec.Code != http.StatusBadGateway {
		t.Errorf("uncached registration with upstream down = %d", rec.Code)
	}
}

func TestGroup(t *testing.T) {
	f, _ := newProxyFixture(t)
	f.mustPush("nuget-hosted", "Remote.Pkg", "3.0.0", "")
	f.mustPush("nuget-hosted", "Local.Only", "1.0.0", "")

	var flat struct {
		Versions []string `json:"versions"`
	}
	f.getJSON("/repository/nuget-group/v3-flatcontainer/remote.pkg/index.json", &flat)
	if strings.Join(flat.Versions, ",") != "1.0.0,2.0.0,3.0.0" {
		t.Errorf("merged versions = %v", flat.Versions)
	}
	var reg regIndex
	f.getJSON("/repository/nuget-group/v3/registration-gz-semver2/remote.pkg/index.json", &reg)
	if got := strings.Join(reg.versions(), ","); got != "1.0.0,2.0.0,3.0.0" {
		t.Errorf("merged registration = %s", got)
	}
	for _, p := range reg.Items[0].Items {
		if !strings.HasPrefix(p.PackageContent, "http://hangar.test/repository/nuget-group/") {
			t.Errorf("group leaf points elsewhere: %s", p.PackageContent)
		}
	}
	for _, p := range []string{
		"v3-flatcontainer/remote.pkg/3.0.0/remote.pkg.3.0.0.nupkg", // hosted member
		"v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg", // proxy member
	} {
		if rec := f.get("/repository/nuget-group/" + p); rec.Code != 200 {
			t.Errorf("group GET %s = %d", p, rec.Code)
		}
	}
	var res searchResult
	f.getJSON("/repository/nuget-group/query?q=&semVerLevel=2.0.0", &res)
	got := map[string]string{}
	for _, d := range res.Data {
		got[d.ID] = d.Version
		if !strings.HasPrefix(d.Registration, "http://hangar.test/repository/nuget-group/") {
			t.Errorf("group search registration = %s", d.Registration)
		}
	}
	if got["Remote.Pkg"] != "3.0.0" || got["Local.Only"] != "1.0.0" || res.TotalHits != 2 {
		t.Errorf("group search = %+v", res)
	}
	if rec := f.get("/repository/nuget-group/v3-flatcontainer/none/index.json"); rec.Code != 404 {
		t.Errorf("group missing = %d", rec.Code)
	}
}

// A reader holding only the group's read privilege restores a package that
// only the group's proxy member has: the proxy caches it on the group's read.
func TestGroupReadFillsProxyWithGroupPrivilegeOnly(t *testing.T) {
	f, _ := newProxyFixture(t)
	f.env.Auth.AddUser("groupie", "pw")
	f.env.AuthZ.Grant("groupie", "repo:nuget-group:read")
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth("groupie", "pw")
		rec := httptest.NewRecorder()
		f.h.ServeHTTP(rec, req)
		return rec
	}

	if rec := get("/repository/nuget-group/v3-flatcontainer/remote.pkg/index.json"); rec.Code != http.StatusOK {
		t.Fatalf("version list through group = %d %s", rec.Code, rec.Body)
	}
	if rec := get("/repository/nuget-group/v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg"); rec.Code != http.StatusOK {
		t.Fatalf("package through group = %d %s", rec.Code, rec.Body)
	}
	proxy, err := f.env.Repos.Get(t.Context(), "nuget-proxy")
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.env.Components.GetAsset(t.Context(), proxy.ID, "v3-flatcontainer/remote.pkg/1.0.0/remote.pkg.1.0.0.nupkg")
	if err != nil {
		t.Fatalf("proxy cache not filled: %v", err)
	}
	if c, err := f.env.Components.GetComponentByID(t.Context(), a.ComponentID); err != nil || c.PublishedBy != "groupie" {
		t.Fatalf("cached component = %+v, %v", c, err)
	}
	if rec := get("/repository/nuget-proxy/v3-flatcontainer/remote.pkg/index.json"); rec.Code != http.StatusForbidden {
		t.Errorf("direct read of the member = %d, want 403", rec.Code)
	}
}

// A feed key in the service index path never reaches a client in an error
// message (the remote failing, or not having the index).
func TestServiceIndexPathStaysOutOfErrors(t *testing.T) {
	f, feed := newProxyFixture(t)
	const key = "k7Qx2mN9vB4cR8tY6wZ1aS3dF5gH0jL"
	f.env.AddRepo(t, &core.Repository{Name: "nuget-vendor", Format: nuget.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: remote},
			Format: map[string]any{"service_index": "/" + key + "/api/v3/index.json"}}})
	f.env.Upstreams.Set("nuget-vendor", feed)
	f.h = formats.NewHandler(f.env.Deps(), nuget.New())
	for _, down := range []bool{false, true} {
		feed.down.Store(down)
		rec := f.get("/repository/nuget-vendor/v3-flatcontainer/remote.pkg/index.json")
		if rec.Code < 500 {
			t.Errorf("down=%v: status %d, want a 5xx", down, rec.Code)
		}
		if strings.Contains(rec.Body.String(), key) {
			t.Errorf("down=%v: the feed key leaked: %s", down, rec.Body)
		}
	}
}
