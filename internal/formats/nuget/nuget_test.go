package nuget_test

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/formats/nuget"
)

// nupkg builds a minimal package archive. extra XML goes inside <metadata>.
func nupkg(t testing.TB, id, version, extra string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(id + ".nuspec")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://schemas.microsoft.com/packaging/2013/05/nuspec.xsd">
  <metadata>
    <id>%s</id>
    <version>%s</version>
    <authors>Alice, Bob</authors>
    <description>The %s library</description>
    <tags>json serializer</tags>
    %s
  </metadata>
</package>`, id, version, id, extra)
	w, err = zw.Create("lib/net10.0/" + id + ".dll")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("binary " + id + " " + version))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fixture struct {
	t     *testing.T
	env   *coretest.Env
	h     http.Handler
	token string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: "nuget-hosted", Format: nuget.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Hosted: &core.HostedConfig{WritePolicy: core.WriteAllowOnce}}})
	token := env.Auth.AddToken("alice")
	env.AuthZ.Grant("alice", "repo:*:read", "repo:*:write", "repo:*:delete")
	env.Auth.AddUser("reader", "pw")
	env.AuthZ.Grant("reader", "repo:*:read")
	return &fixture{t: t, env: env, h: formats.NewHandler(env.Deps(), nuget.New()), token: token}
}

func (f *fixture) do(method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

// get reads as alice via Basic auth with her token.
func (f *fixture) get(path string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetBasicAuth("alice", f.token)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func (f *fixture) getJSON(path string, v any) {
	f.t.Helper()
	rec := f.get(path)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		f.t.Fatalf("GET %s: %v", path, err)
	}
}

func (f *fixture) push(repo string, pkg []byte, key string) *httptest.ResponseRecorder {
	f.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("package", "package.nupkg")
	_, _ = fw.Write(pkg)
	_ = mw.Close()
	hdr := map[string]string{"Content-Type": mw.FormDataContentType()}
	if key != "" {
		hdr["X-NuGet-ApiKey"] = key
	}
	return f.do(http.MethodPut, "/repository/"+repo+"/api/v2/package", &buf, hdr)
}

func (f *fixture) mustPush(repo, id, version, extra string) {
	f.t.Helper()
	if rec := f.push(repo, nupkg(f.t, id, version, extra), f.token); rec.Code != http.StatusCreated {
		f.t.Fatalf("push %s %s = %d %s", id, version, rec.Code, rec.Body)
	}
}

func TestServiceIndex(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "nuget-proxy", Format: nuget.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://api.nuget.org"}}})
	types := func(repo string) map[string]string {
		var doc struct {
			Version   string `json:"version"`
			Resources []struct {
				ID   string `json:"@id"`
				Type string `json:"@type"`
			} `json:"resources"`
		}
		rec := f.do(http.MethodGet, "/repository/"+repo+"/index.json", nil, nil) // anonymous
		if rec.Code != 200 {
			t.Fatalf("index %s = %d", repo, rec.Code)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		if doc.Version != "3.0.0" {
			t.Errorf("version = %q", doc.Version)
		}
		out := map[string]string{}
		for _, r := range doc.Resources {
			out[r.Type] = r.ID
		}
		return out
	}
	hosted := types("nuget-hosted")
	base := "http://hangar.test/repository/nuget-hosted/"
	for typ, id := range map[string]string{
		"PackageBaseAddress/3.0.0":   base + "v3-flatcontainer/",
		"RegistrationsBaseUrl":       base + "v3/registration/",
		"RegistrationsBaseUrl/3.4.0": base + "v3/registration-gz/",
		"RegistrationsBaseUrl/3.6.0": base + "v3/registration-gz-semver2/",
		"SearchQueryService/3.5.0":   base + "query",
		"PackagePublish/2.0.0":       base + "api/v2/package",
		"SymbolPackagePublish/4.9.0": base + "api/v2/symbolpackage",
	} {
		if hosted[typ] != id {
			t.Errorf("%s = %q, want %q", typ, hosted[typ], id)
		}
	}
	if _, ok := types("nuget-proxy")["PackagePublish/2.0.0"]; ok {
		t.Error("proxy advertises PackagePublish")
	}
	if rec := f.do(http.MethodHead, "/repository/nuget-hosted/index.json", nil, nil); rec.Code != 200 {
		t.Errorf("HEAD index = %d", rec.Code)
	}
}

func TestPushAndDownload(t *testing.T) {
	f := newFixture(t)
	pkg := nupkg(t, "Acme.Json", "1.0.0-Beta+build.7", `<dependencies><group targetFramework="net10.0"><dependency id="Other.Lib" version="2.0" /></group></dependencies>`)

	tests := []struct {
		name string
		repo string
		body []byte
		key  string
		want int
	}{
		{"no key", "nuget-hosted", pkg, "", http.StatusUnauthorized},
		{"bad key", "nuget-hosted", pkg, "hgr_nope", http.StatusUnauthorized},
		{"not a zip", "nuget-hosted", []byte("hello"), f.token, http.StatusBadRequest},
		{"no nuspec", "nuget-hosted", zipOf(t, map[string]string{"lib/a.dll": "x"}), f.token, http.StatusBadRequest},
		{"bad id", "nuget-hosted", nupkg(t, "bad id", "1.0.0", ""), f.token, http.StatusBadRequest},
		{"bad version", "nuget-hosted", nupkg(t, "Good.Id", "1.0.0-", ""), f.token, http.StatusBadRequest},
		{"ok", "nuget-hosted", pkg, f.token, http.StatusCreated},
		{"same bytes again, allow_once", "nuget-hosted", pkg, f.token, http.StatusConflict},
		{"different bytes, allow_once", "nuget-hosted", nupkg(t, "Acme.Json", "1.0.0-beta+other", ""), f.token, http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.push(tt.repo, tt.body, tt.key)
			if rec.Code != tt.want {
				t.Fatalf("push = %d %s, want %d", rec.Code, rec.Body, tt.want)
			}
			if rec.Code == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without challenge")
			}
		})
	}
	reader := map[string]string{"Authorization": "Basic cmVhZGVyOnB3"} // reader:pw
	if rec := f.push("nuget-hosted", pkg, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous push = %d", rec.Code)
	}
	var buf bytes.Buffer
	buf.Write(pkg)
	if rec := f.do(http.MethodPut, "/repository/nuget-hosted/api/v2/package", &buf, reader); rec.Code != http.StatusForbidden {
		t.Errorf("reader push = %d", rec.Code)
	}

	var flat struct {
		Versions []string `json:"versions"`
	}
	f.getJSON("/repository/nuget-hosted/v3-flatcontainer/ACME.JSON/index.json", &flat)
	if len(flat.Versions) != 1 || flat.Versions[0] != "1.0.0-beta" {
		t.Errorf("versions = %v", flat.Versions)
	}
	rec := f.get("/repository/nuget-hosted/v3-flatcontainer/acme.json/1.0.0-beta/acme.json.1.0.0-beta.nupkg")
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), pkg) {
		t.Fatalf("download = %d (%d bytes)", rec.Code, rec.Body.Len())
	}
	if f.env.Components.Downloads() != 1 {
		t.Errorf("downloads recorded = %d", f.env.Components.Downloads())
	}
	rec = f.get("/repository/nuget-hosted/v3-flatcontainer/acme.json/1.0.0-beta/acme.json.nuspec")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<id>Acme.Json</id>") {
		t.Errorf("nuspec = %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{
		"v3-flatcontainer/missing/index.json",
		"v3-flatcontainer/acme.json/9.9.9/acme.json.9.9.9.nupkg",
		"v3-flatcontainer/acme.json/1.0.0-beta/other.1.0.0-beta.nupkg",
		"v3-flatcontainer/acme.json/not-a-version/acme.json.nuspec",
	} {
		if rec := f.get("/repository/nuget-hosted/" + p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	if rec := f.do(http.MethodGet, "/repository/nuget-hosted/v3-flatcontainer/acme.json/index.json", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous read of private repo = %d", rec.Code)
	}

	// Component and assets as the rest of Hangar sees them.
	repo, _ := f.env.Repos.Get(t.Context(), "nuget-hosted")
	c, err := f.env.Components.GetComponent(t.Context(), repo.ID, "", "acme.json", "1.0.0-beta")
	if err != nil {
		t.Fatal(err)
	}
	if c.Attributes["id"] != "Acme.Json" || c.Attributes["version"] != "1.0.0-Beta+build.7" || c.Attributes["listed"] != true {
		t.Errorf("attributes = %v", c.Attributes)
	}
	a, err := f.env.Components.GetAsset(t.Context(), repo.ID, "v3-flatcontainer/acme.json/1.0.0-beta/acme.json.1.0.0-beta.nupkg")
	if err != nil || len(a.Checksums["sha512"]) != 128 {
		t.Fatalf("asset = %+v, %v", a, err)
	}
	coords := nuget.New().Coordinates(a)
	if coords.Name != "Acme.Json" || coords.Version != "1.0.0-Beta+build.7" {
		t.Errorf("coordinates = %+v", coords)
	}
	if n := countEvents(f.env, core.EventComponentPublished); n < 1 {
		t.Errorf("published events = %d", n)
	}
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	return buf.Bytes()
}

func countEvents(env *coretest.Env, typ string) int {
	return len(env.Events.Published(typ))
}

type regIndex struct {
	Count int `json:"count"`
	Items []struct {
		ID    string `json:"@id"`
		Count int    `json:"count"`
		Lower string `json:"lower"`
		Upper string `json:"upper"`
		Items []struct {
			ID             string `json:"@id"`
			PackageContent string `json:"packageContent"`
			CatalogEntry   struct {
				ID               string `json:"id"`
				Version          string `json:"version"`
				Listed           bool   `json:"listed"`
				Published        string `json:"published"`
				PackageContent   string `json:"packageContent"`
				DependencyGroups []struct {
					TargetFramework string `json:"targetFramework"`
					Dependencies    []struct {
						ID           string `json:"id"`
						Range        string `json:"range"`
						Registration string `json:"registration"`
					} `json:"dependencies"`
				} `json:"dependencyGroups"`
			} `json:"catalogEntry"`
		} `json:"items"`
	} `json:"items"`
}

func (r regIndex) versions() []string {
	var out []string
	for _, p := range r.Items {
		for _, l := range p.Items {
			out = append(out, l.CatalogEntry.Version)
		}
	}
	return out
}

func TestRegistration(t *testing.T) {
	f := newFixture(t)
	f.mustPush("nuget-hosted", "Acme.Core", "1.0.0", `<dependencies><dependency id="Dep.A" version="1.0" /></dependencies>`)
	f.mustPush("nuget-hosted", "Acme.Core", "2.0.0-beta.1", "")
	f.mustPush("nuget-hosted", "Acme.Core", "1.5.0-rc", "")

	var sv1, sv2 regIndex
	f.getJSON("/repository/nuget-hosted/v3/registration/acme.core/index.json", &sv1)
	if got := strings.Join(sv1.versions(), ","); got != "1.0.0,1.5.0-rc" {
		t.Errorf("semver1 hive = %s", got)
	}
	f.getJSON("/repository/nuget-hosted/v3/registration-gz-semver2/Acme.Core/index.json", &sv2)
	if got := strings.Join(sv2.versions(), ","); got != "1.0.0,1.5.0-rc,2.0.0-beta.1" {
		t.Errorf("semver2 hive = %s", got)
	}
	p := sv2.Items[0]
	if sv2.Count != 1 || p.Count != 3 || p.Lower != "1.0.0" || p.Upper != "2.0.0-beta.1" {
		t.Errorf("page = %+v", p)
	}
	leaf := p.Items[0]
	base := "http://hangar.test/repository/nuget-hosted/"
	if leaf.PackageContent != base+"v3-flatcontainer/acme.core/1.0.0/acme.core.1.0.0.nupkg" ||
		leaf.CatalogEntry.PackageContent != leaf.PackageContent || leaf.CatalogEntry.ID != "Acme.Core" || !leaf.CatalogEntry.Listed {
		t.Errorf("leaf = %+v", leaf)
	}
	dep := leaf.CatalogEntry.DependencyGroups[0].Dependencies[0]
	if dep.ID != "Dep.A" || dep.Range != "[1.0.0, )" || dep.Registration != base+"v3/registration-gz-semver2/dep.a/index.json" {
		t.Errorf("dependency = %+v", dep)
	}

	// gzip hives compress for clients that accept it.
	req := httptest.NewRequest(http.MethodGet, "/repository/nuget-hosted/v3/registration-gz/acme.core/index.json", nil)
	req.SetBasicAuth("alice", f.token)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("gz hive encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	var gz regIndex
	if err := json.NewDecoder(zr).Decode(&gz); err != nil || len(gz.versions()) != 2 {
		t.Errorf("gz hive = %v %v", gz.versions(), err)
	}

	// Leaf document.
	var l struct {
		ID             string `json:"@id"`
		Listed         bool   `json:"listed"`
		PackageContent string `json:"packageContent"`
		Registration   string `json:"registration"`
	}
	f.getJSON("/repository/nuget-hosted/v3/registration-gz-semver2/acme.core/2.0.0-beta.1.json", &l)
	if !l.Listed || l.Registration != base+"v3/registration-gz-semver2/acme.core/index.json" ||
		l.ID != base+"v3/registration-gz-semver2/acme.core/2.0.0-beta.1.json" {
		t.Errorf("leaf doc = %+v", l)
	}
	for _, p := range []string{
		"v3/registration/acme.core/2.0.0-beta.1.json", // semver2 version hidden from the semver1 hive
		"v3/registration/missing/index.json",
		"v3/nope/acme.core/index.json",
		"v3/registration/acme.core/9.0.0.json",
	} {
		if rec := f.get("/repository/nuget-hosted/" + p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d", p, rec.Code)
		}
	}
}

func TestRegistrationPaging(t *testing.T) {
	f := newFixture(t)
	for i := range 130 {
		f.mustPush("nuget-hosted", "Many", fmt.Sprintf("1.0.%d", i), "")
	}
	var idx regIndex
	f.getJSON("/repository/nuget-hosted/v3/registration-gz-semver2/many/index.json", &idx)
	if idx.Count != 3 || len(idx.Items) != 3 {
		t.Fatalf("pages = %d", idx.Count)
	}
	total := 0
	for _, p := range idx.Items {
		if p.Items != nil {
			t.Fatal("pages must not be inlined at 130 versions")
		}
		path := strings.TrimPrefix(p.ID, "http://hangar.test")
		var page struct {
			Count  int               `json:"count"`
			Parent string            `json:"parent"`
			Items  []json.RawMessage `json:"items"`
		}
		f.getJSON(path, &page)
		if page.Count != len(page.Items) || page.Parent == "" {
			t.Errorf("page %s = %d/%d", path, page.Count, len(page.Items))
		}
		total += page.Count
	}
	if total != 130 || idx.Items[0].Lower != "1.0.0" || idx.Items[2].Upper != "1.0.129" {
		t.Errorf("total = %d, bounds %s..%s", total, idx.Items[0].Lower, idx.Items[2].Upper)
	}
	if rec := f.get("/repository/nuget-hosted/v3/registration/many/page/1.0.0/1.0.5.json"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown page = %d", rec.Code)
	}

	// Below 128 versions every page is inlined.
	for i := range 70 {
		f.mustPush("nuget-hosted", "Some", fmt.Sprintf("2.0.%d", i), "")
	}
	f.getJSON("/repository/nuget-hosted/v3/registration/some/index.json", &idx)
	if idx.Count != 2 || len(idx.Items[0].Items) != 64 || len(idx.Items[1].Items) != 6 {
		t.Errorf("inline pages = %d", idx.Count)
	}
}

type searchResult struct {
	TotalHits int `json:"totalHits"`
	Data      []struct {
		ID           string   `json:"id"`
		Version      string   `json:"version"`
		Registration string   `json:"registration"`
		Authors      []string `json:"authors"`
		Versions     []struct {
			Version string `json:"version"`
			ID      string `json:"@id"`
		} `json:"versions"`
	} `json:"data"`
}

func (r searchResult) ids() string {
	var out []string
	for _, d := range r.Data {
		out = append(out, d.ID+"@"+d.Version)
	}
	return strings.Join(out, ",")
}

func TestSearch(t *testing.T) {
	f := newFixture(t)
	f.mustPush("nuget-hosted", "Acme.Json", "1.0.0", "")
	f.mustPush("nuget-hosted", "Acme.Json", "1.1.0-beta", "")
	f.mustPush("nuget-hosted", "Acme.Json", "2.0.0-beta.1", "")
	f.mustPush("nuget-hosted", "Acme.Xml", "3.0.0", "<title>XML tools</title>")
	f.mustPush("nuget-hosted", "Zeta", "0.1.0", `<tags>other</tags>`)
	f.mustPush("nuget-hosted", "Hidden", "1.0.0", "")
	if rec := f.do(http.MethodDelete, "/repository/nuget-hosted/api/v2/package/hidden/1.0.0", nil,
		map[string]string{"X-NuGet-ApiKey": f.token}); rec.Code != http.StatusNoContent {
		t.Fatalf("unlist = %d %s", rec.Code, rec.Body)
	}

	tests := []struct{ query, want string }{
		{"", "Acme.Json@1.0.0,Acme.Xml@3.0.0,Zeta@0.1.0"},
		{"q=&prerelease=true", "Acme.Json@1.1.0-beta,Acme.Xml@3.0.0,Zeta@0.1.0"},
		{"q=&prerelease=true&semVerLevel=2.0.0", "Acme.Json@2.0.0-beta.1,Acme.Xml@3.0.0,Zeta@0.1.0"},
		{"q=acme", "Acme.Json@1.0.0,Acme.Xml@3.0.0"},
		{"q=xml+tools", "Acme.Xml@3.0.0"},
		{"q=packageid:acme.json", "Acme.Json@1.0.0"}, //nolint:misspell // NuGet search field name
		{"q=tags:other", "Zeta@0.1.0"},
		{"q=author:bob", "Acme.Json@1.0.0,Acme.Xml@3.0.0,Zeta@0.1.0"},
		{"q=hidden", ""},
		{"q=zeta", "Zeta@0.1.0"},
		{"skip=1&take=1", "Acme.Xml@3.0.0"},
		{"q=&packageType=DotnetTool", ""},
		{"q=&packageType=Dependency&take=1", "Acme.Json@1.0.0"},
	}
	for _, tt := range tests {
		var res searchResult
		f.getJSON("/repository/nuget-hosted/query?"+tt.query, &res)
		if got := res.ids(); got != tt.want {
			t.Errorf("query %q = %s, want %s", tt.query, got, tt.want)
		}
	}
	var res searchResult
	f.getJSON("/repository/nuget-hosted/query?q=acme.json&prerelease=true&semVerLevel=2.0.0", &res)
	d := res.Data[0]
	if res.TotalHits != 1 || len(d.Versions) != 3 || d.Authors[1] != "Bob" ||
		d.Registration != "http://hangar.test/repository/nuget-hosted/v3/registration-gz-semver2/acme.json/index.json" ||
		d.Versions[2].ID != "http://hangar.test/repository/nuget-hosted/v3/registration-gz-semver2/acme.json/2.0.0-beta.1.json" {
		t.Errorf("result = %+v", res)
	}
}

func TestUnlistRelistDelete(t *testing.T) {
	f := newFixture(t)
	f.mustPush("nuget-hosted", "Acme.Lib", "1.0.0", "")
	key := map[string]string{"X-NuGet-ApiKey": f.token}
	reader := map[string]string{"Authorization": "Basic cmVhZGVyOnB3"}

	if rec := f.do(http.MethodDelete, "/repository/nuget-hosted/api/v2/package/Acme.Lib/1.0.0", nil, reader); rec.Code != http.StatusForbidden {
		t.Errorf("reader unlist = %d", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/repository/nuget-hosted/api/v2/package/Acme.Lib/9.0.0", nil, key); rec.Code != http.StatusNotFound {
		t.Errorf("unlist missing = %d", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/repository/nuget-hosted/api/v2/package/Acme.Lib/1.0", nil, key); rec.Code != http.StatusNoContent {
		t.Fatalf("unlist = %d %s", rec.Code, rec.Body)
	}
	var reg regIndex
	f.getJSON("/repository/nuget-hosted/v3/registration/acme.lib/index.json", &reg)
	e := reg.Items[0].Items[0].CatalogEntry
	if e.Listed || !strings.HasPrefix(e.Published, "1900-") {
		t.Errorf("unlisted entry = %+v", e)
	}
	// Still downloadable by exact version.
	if rec := f.get("/repository/nuget-hosted/v3-flatcontainer/acme.lib/1.0.0/acme.lib.1.0.0.nupkg"); rec.Code != 200 {
		t.Errorf("download unlisted = %d", rec.Code)
	}
	if !hasAudit(f.env, "component.unlist") {
		t.Error("unlist not audited")
	}
	if rec := f.do(http.MethodPost, "/repository/nuget-hosted/api/v2/package/acme.lib/1.0.0", nil, key); rec.Code != http.StatusOK {
		t.Fatalf("relist = %d", rec.Code)
	}
	f.getJSON("/repository/nuget-hosted/v3/registration/acme.lib/index.json", &reg)
	if !reg.Items[0].Items[0].CatalogEntry.Listed || !hasAudit(f.env, "component.relist") {
		t.Error("relist did not list the version")
	}

	// delete_mode=delete removes the version.
	f.env.AddRepo(t, &core.Repository{Name: "nuget-hard", Format: nuget.Name, Type: core.RepoHosted,
		Config: core.RepoConfig{Format: map[string]any{"delete_mode": "delete"}}})
	f.mustPush("nuget-hard", "Acme.Lib", "1.0.0", "")
	if rec := f.do(http.MethodDelete, "/repository/nuget-hard/api/v2/package/acme.lib/1.0.0", nil, key); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := f.get("/repository/nuget-hard/v3-flatcontainer/acme.lib/index.json"); rec.Code != http.StatusNotFound {
		t.Errorf("after hard delete = %d", rec.Code)
	}
}

func hasAudit(env *coretest.Env, action string) bool {
	return len(env.Audit.Events(action)) > 0
}

func TestQuarantine(t *testing.T) {
	f := newFixture(t)
	repo, _ := f.env.Repos.Get(t.Context(), "nuget-hosted")
	d, n, _ := f.env.Blob.Put(t.Context(), bytes.NewReader([]byte("pkg")))
	ctx := coretest.AsUser(t.Context(), "alice")
	_, _, err := f.env.Components.Publish(ctx, repo, core.PublishRequest{
		Component: &core.Component{Name: "bad.pkg", Version: "1.0.0", Quarantined: true},
		Assets:    []*core.Asset{{Path: "v3-flatcontainer/bad.pkg/1.0.0/bad.pkg.1.0.0.nupkg", BlobDigest: d, Size: n}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.get("/repository/nuget-hosted/v3-flatcontainer/bad.pkg/1.0.0/bad.pkg.1.0.0.nupkg"); rec.Code != http.StatusForbidden {
		t.Errorf("quarantined download = %d", rec.Code)
	}
}

func TestValidate(t *testing.T) {
	f := nuget.New()
	ok := []*core.Repository{
		{Name: "a", Format: "nuget", Type: core.RepoHosted},
		{Name: "a", Format: "nuget", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"delete_mode": "delete"}}},
		{Name: "a", Format: "nuget", Type: core.RepoProxy, Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://api.nuget.org"}}},
		{Name: "a", Format: "nuget", Type: core.RepoProxy, Config: core.RepoConfig{
			Proxy: &core.ProxyConfig{RemoteURL: "https://feed.example/nuget"}, Format: map[string]any{"service_index": "index.json"}}},
	}
	for i, r := range ok {
		if err := f.Validate(r); err != nil {
			t.Errorf("ok[%d]: %v", i, err)
		}
	}
	bad := []*core.Repository{
		{Name: "a", Format: "npm", Type: core.RepoHosted},
		{Name: "a", Format: "nuget", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"delete_mode": "purge"}}},
		{Name: "a", Format: "nuget", Type: core.RepoHosted, Config: core.RepoConfig{Format: map[string]any{"delete_mode": 3}}},
		{Name: "a", Format: "nuget", Type: core.RepoProxy, Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://api.nuget.org/v3/index.json"}}},
		{Name: "a", Format: "nuget", Type: core.RepoProxy, Config: core.RepoConfig{
			Proxy: &core.ProxyConfig{RemoteURL: "https://x.example"}, Format: map[string]any{"service_index": "../index.json"}}},
	}
	for i, r := range bad {
		if err := f.Validate(r); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("bad[%d]: %v", i, err)
		}
	}
}

func TestPushRouting(t *testing.T) {
	f := newFixture(t)
	f.env.AddRepo(t, &core.Repository{Name: "nuget-proxy", Format: nuget.Name, Type: core.RepoProxy,
		Config: core.RepoConfig{Proxy: &core.ProxyConfig{RemoteURL: "https://api.nuget.org"}}})
	f.env.AddRepo(t, &core.Repository{Name: "nuget-ro-group", Format: nuget.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"nuget-hosted"}}}})
	f.env.AddRepo(t, &core.Repository{Name: "nuget-group", Format: nuget.Name, Type: core.RepoGroup,
		Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"nuget-hosted"}, WriteMember: "nuget-hosted"}}})
	pkg := nupkg(t, "Routed", "1.0.0", "")
	for repo, want := range map[string]int{"nuget-proxy": 405, "nuget-ro-group": 405, "nuget-group": 201} {
		if rec := f.push(repo, pkg, f.token); rec.Code != want {
			t.Errorf("push to %s = %d, want %d", repo, rec.Code, want)
		}
	}
	var flat struct {
		Versions []string `json:"versions"`
	}
	f.getJSON("/repository/nuget-hosted/v3-flatcontainer/routed/index.json", &flat)
	if len(flat.Versions) != 1 {
		t.Errorf("group push did not land in the write member: %v", flat.Versions)
	}
}

func TestReadNupkg(t *testing.T) {
	pkg := nupkg(t, "Acme.Full", "1.2.3.0", `<title>Full</title><projectUrl>https://acme.example</projectUrl>
		<license type="expression">MIT</license><requireLicenseAcceptance>true</requireLicenseAcceptance>
		<packageTypes><packageType name="DotnetTool" /></packageTypes>
		<dependencies><dependency id="Plain" version="[1.0,2.0)" /></dependencies>`)
	md, raw, err := nuget.ReadNupkg(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	if md.ID != "Acme.Full" || md.Version != "1.2.3" || md.Title != "Full" || md.LicenseExpression != "MIT" ||
		!md.RequireLicenseAcceptance || md.PackageTypes[0].Name != "DotnetTool" || len(md.Tags) != 2 ||
		md.DependencyGroups[0].Dependencies[0].Range != "[1.0,2.0)" || !bytes.Contains(raw, []byte("Acme.Full")) {
		t.Errorf("metadata = %+v", md)
	}
	two := zipOf(t, map[string]string{"a.nuspec": "<package/>", "b.nuspec": "<package/>"})
	if _, _, err := nuget.ReadNupkg(bytes.NewReader(two), int64(len(two))); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("two nuspecs: %v", err)
	}
	bad := zipOf(t, map[string]string{"a.nuspec": "<package><metadata><id>x"})
	if _, _, err := nuget.ReadNupkg(bytes.NewReader(bad), int64(len(bad))); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("broken xml: %v", err)
	}
}
