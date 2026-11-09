package helm_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

func TestUploadAndIndex(t *testing.T) {
	f := newFixture(t)
	a := chartTgz(t, "web", "1.0.0", "")
	b := chartTgz(t, "web", "1.10.0", "")
	pre := chartTgz(t, "web", "2.0.0-rc.1", "")
	other := chartTgz(t, "api-gateway", "0.3.1", "")

	f.mustUpload(hostedName, a)
	// ChartMuseum multipart, as helm cm-push sends it.
	body, ct := multipartBody(t, map[string][]byte{"chart": b})
	if rec := f.do(http.MethodPost, "/repository/"+hostedName+"/api/charts", body, ct); rec.Code != http.StatusCreated ||
		!strings.Contains(rec.Body.String(), `"saved":true`) {
		t.Fatalf("multipart upload = %d %s", rec.Code, rec.Body)
	}
	// Plain PUT, as curl --upload-file sends it.
	if rec := f.do(http.MethodPut, "/repository/"+hostedName+"/web-2.0.0-rc.1.tgz", pre, ""); rec.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodPut, "/repository/"+hostedName+"/api-gateway-0.3.1.tgz", other, ""); rec.Code != http.StatusCreated {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}

	idx := f.index(hostedName)
	if idx.APIVersion != "v1" || idx.ServerInfo.ContextPath != "/repository/"+hostedName {
		t.Fatalf("index header = %+v", idx)
	}
	web := idx.Entries["web"]
	if len(web) != 3 || web[0].Version != "2.0.0-rc.1" || web[1].Version != "1.10.0" || web[2].Version != "1.0.0" {
		t.Fatalf("web versions = %+v", web)
	}
	e := web[2]
	if e.URLs[0] != "web/1.0.0/web-1.0.0.tgz" || e.Digest != sha(a) || e.Created.IsZero() ||
		e.AppVersion != "1.16.0" || e.APIVersion != "v2" || e.Description != "The web chart" || e.Maintainers[0].Name != "Alice" {
		t.Fatalf("entry = %+v", e)
	}
	if len(idx.Entries["api-gateway"]) != 1 {
		t.Fatalf("api-gateway = %+v", idx.Entries["api-gateway"])
	}

	// The archive is served at the index URL and flat, and downloads are recorded.
	for _, p := range []string{"web/1.0.0/web-1.0.0.tgz", "web-1.0.0.tgz", "web-2.0.0-rc.1.tgz", "api-gateway-0.3.1.tgz"} {
		rec := f.do(http.MethodGet, "/repository/"+hostedName+"/"+p, nil, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", p, rec.Code, rec.Body)
		}
	}
	if rec := f.do(http.MethodGet, "/repository/"+hostedName+"/web/1.0.0/web-1.0.0.tgz", nil, ""); rec.Body.String() != string(a) ||
		rec.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("archive bytes or type differ: %s", rec.Header())
	}
	if f.env.Components.Downloads() == 0 {
		t.Fatal("no download recorded")
	}

	// Component attributes carry what the UI shows.
	repo, _ := f.env.Repos.Get(context.Background(), hostedName)
	c, err := f.env.Components.GetComponent(context.Background(), repo.ID, "", "web", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if c.Attributes["license"] != "Apache-2.0" || c.Attributes["source_url"] != "https://git.example.org/acme/web" ||
		c.Attributes["description"] != "The web chart" {
		t.Fatalf("attributes = %v", c.Attributes)
	}
	// Publishing is audited by the component service.
	if len(f.env.Audit.Events("component.publish")) != 4 {
		t.Fatalf("audit = %v", f.env.Audit.Events("component.publish"))
	}
}

func TestIndexETagAndCacheInvalidation(t *testing.T) {
	f := newFixture(t)
	f.mustUpload(hostedName, chartTgz(t, "web", "1.0.0", ""))
	rec := f.do(http.MethodGet, "/repository/"+hostedName+"/index.yaml", nil, "")
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	req := f.do(http.MethodGet, "/repository/"+hostedName+"/index.yaml", nil, "")
	if req.Header().Get("ETag") != etag {
		t.Fatal("cached index changed")
	}
	// A deletion outside the format (UI, cleanup policy) reaches the index
	// through the component.deleted event.
	repo, _ := f.env.Repos.Get(context.Background(), hostedName)
	c, _ := f.env.Components.GetComponent(context.Background(), repo.ID, "", "web", "1.0.0")
	if err := f.env.Components.DeleteComponent(coretest.AsUser(context.Background(), "alice"), repo, c.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.index(hostedName).Entries["web"]; len(got) != 0 {
		t.Fatalf("deleted version still listed: %+v", got)
	}
}

func TestChartValidation(t *testing.T) {
	f := newFixture(t)
	cases := map[string]string{
		"no apiVersion":   "name: x\nversion: 1.0.0\n",
		"bad apiVersion":  "apiVersion: v9\nname: x\nversion: 1.0.0\n",
		"no version":      "apiVersion: v2\nname: x\n",
		"not semver":      "apiVersion: v2\nname: x\nversion: one\n",
		"path name":       "apiVersion: v2\nname: ../x\nversion: 1.0.0\n",
		"bad type":        "apiVersion: v2\nname: x\nversion: 1.0.0\ntype: plugin\n",
		"v1 library":      "apiVersion: v1\nname: x\nversion: 1.0.0\ntype: library\n",
		"multiline app":   "apiVersion: v2\nname: x\nversion: 1.0.0\nappVersion: |\n  a\n  b\n",
		"not yaml":        "apiVersion: [v2\n",
		"dependency name": "apiVersion: v2\nname: x\nversion: 1.0.0\ndependencies:\n  - version: 1.0.0\n",
	}
	for name, doc := range cases {
		rec := f.do(http.MethodPost, "/repository/"+hostedName+"/api/charts", chartTgz(t, "x", "1.0.0", doc), "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("%s: = %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.do(http.MethodPost, "/repository/"+hostedName+"/api/charts", []byte("not a tgz"), ""); rec.Code != http.StatusBadRequest {
		t.Errorf("garbage = %d", rec.Code)
	}
	// Unquoted numeric versions keep their text; v1 charts are accepted.
	rec := f.do(http.MethodPost, "/repository/"+hostedName+"/api/charts",
		chartTgz(t, "legacy", "1.0", "apiVersion: v1\nname: legacy\nversion: 1.0\nappVersion: 2.10\n"), "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("v1 chart = %d %s", rec.Code, rec.Body)
	}
	if e := f.index(hostedName).Entries["legacy"]; len(e) != 1 || e[0].Version != "1.0" || e[0].AppVersion != "2.10" {
		t.Fatalf("legacy = %+v", e)
	}
	// PUT under a name that does not match Chart.yaml is refused.
	if rec := f.do(http.MethodPut, "/repository/"+hostedName+"/other-1.0.0.tgz", chartTgz(t, "web", "1.0.0", ""), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched PUT = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodPut, "/repository/"+hostedName+"/notes.txt", []byte("x"), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT of a non-chart = %d", rec.Code)
	}
}

func TestWritePolicies(t *testing.T) {
	f := newFixture(t)
	v1 := chartTgz(t, "web", "1.0.0", "")
	v1b := chartTgz(t, "web", "1.0.0", "apiVersion: v2\nname: web\nversion: 1.0.0\ndescription: changed\n")
	post := func(repo string, b []byte, query string) int {
		t.Helper()
		return f.do(http.MethodPost, "/repository/"+repo+"/api/charts"+query, b, "").Code
	}

	// allow_once: same bytes are idempotent, different bytes never overwrite.
	if c := post(once, v1, ""); c != http.StatusCreated {
		t.Fatalf("first upload = %d", c)
	}
	if c := post(once, v1, ""); c != http.StatusCreated {
		t.Fatalf("identical re-upload = %d", c)
	}
	if c := post(once, v1b, ""); c != http.StatusConflict {
		t.Fatalf("overwrite on allow_once = %d", c)
	}
	if c := post(once, v1b, "?force"); c != http.StatusConflict {
		t.Fatalf("forced overwrite on allow_once = %d", c)
	}

	// allow: a different archive needs ?force (helm cm-push --force).
	if c := post(hostedName, v1, ""); c != http.StatusCreated {
		t.Fatalf("first upload = %d", c)
	}
	prov := provFor("web", "1.0.0", v1)
	if rec := f.do(http.MethodPut, "/repository/"+hostedName+"/web-1.0.0.tgz.prov", prov, ""); rec.Code != http.StatusCreated {
		t.Fatalf("prov = %d %s", rec.Code, rec.Body)
	}
	rec := f.do(http.MethodPost, "/repository/"+hostedName+"/api/charts", v1b, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "force") {
		t.Fatalf("overwrite without force = %d %s", rec.Code, rec.Body)
	}
	if c := post(hostedName, v1b, "?force"); c != http.StatusCreated {
		t.Fatalf("forced overwrite = %d", c)
	}
	if e := f.index(hostedName).Entries["web"][0]; e.Digest != sha(v1b) || e.Description != "changed" {
		t.Fatalf("after overwrite = %+v", e)
	}
	// The provenance file signed the old archive and is gone.
	if c := f.do(http.MethodGet, "/repository/"+hostedName+"/web/1.0.0/web-1.0.0.tgz.prov", nil, "").Code; c != http.StatusNotFound {
		t.Fatalf("stale prov = %d", c)
	}
}

func TestProvenance(t *testing.T) {
	f := newFixture(t)
	tgz := chartTgz(t, "web", "1.0.0", "")
	prov := provFor("web", "1.0.0", tgz)
	// A provenance file needs its chart first.
	if rec := f.do(http.MethodPost, "/repository/"+hostedName+"/api/prov", prov, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("prov before chart = %d %s", rec.Code, rec.Body)
	}
	// Chart and prov in one multipart request.
	body, ct := multipartBody(t, map[string][]byte{"chart": tgz, "prov": prov})
	if rec := f.do(http.MethodPost, "/repository/"+hostedName+"/api/charts", body, ct); rec.Code != http.StatusCreated {
		t.Fatalf("chart+prov = %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{"web/1.0.0/web-1.0.0.tgz.prov", "web-1.0.0.tgz.prov"} {
		rec := f.do(http.MethodGet, "/repository/"+hostedName+"/"+p, nil, "")
		if rec.Code != http.StatusOK || rec.Body.String() != string(prov) {
			t.Fatalf("GET %s = %d", p, rec.Code)
		}
	}
	// A provenance file for other bytes is refused.
	bad := provFor("web", "1.0.0", []byte("other"))
	if rec := f.do(http.MethodPost, "/repository/"+hostedName+"/api/prov", bad, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched prov = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(http.MethodPost, "/repository/"+hostedName+"/api/prov", []byte("not signed"), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage prov = %d", rec.Code)
	}
}

func TestChartMuseumAPI(t *testing.T) {
	f := newFixture(t)
	f.mustUpload(hostedName, chartTgz(t, "web", "1.0.0", ""))
	f.mustUpload(hostedName, chartTgz(t, "web", "1.2.0", ""))

	var all map[string][]map[string]any
	decodeJSON(t, f.do(http.MethodGet, "/repository/"+hostedName+"/api/charts", nil, ""), &all)
	if len(all["web"]) != 2 || all["web"][0]["version"] != "1.2.0" || all["web"][0]["urls"].([]any)[0] != "web/1.2.0/web-1.2.0.tgz" {
		t.Fatalf("api/charts = %v", all)
	}
	var list []map[string]any
	decodeJSON(t, f.do(http.MethodGet, "/repository/"+hostedName+"/api/charts/web", nil, ""), &list)
	if len(list) != 2 {
		t.Fatalf("api/charts/web = %v", list)
	}
	var one map[string]any
	decodeJSON(t, f.do(http.MethodGet, "/repository/"+hostedName+"/api/charts/web/latest", nil, ""), &one)
	if one["version"] != "1.2.0" || one["digest"] == "" || one["created"] == "" {
		t.Fatalf("latest = %v", one)
	}
	if c := f.do(http.MethodGet, "/repository/"+hostedName+"/api/charts/nope", nil, "").Code; c != http.StatusNotFound {
		t.Fatalf("missing chart = %d", c)
	}
	if c := f.do(http.MethodGet, "/repository/"+hostedName+"/api/charts/web/9.9.9", nil, "").Code; c != http.StatusNotFound {
		t.Fatalf("missing version = %d", c)
	}

	rec := f.do(http.MethodDelete, "/repository/"+hostedName+"/api/charts/web/1.0.0", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"deleted":true`) {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if c := f.do(http.MethodDelete, "/repository/"+hostedName+"/api/charts/web/1.0.0", nil, "").Code; c != http.StatusNotFound {
		t.Fatalf("second delete = %d", c)
	}
	if e := f.index(hostedName).Entries["web"]; len(e) != 1 || e[0].Version != "1.2.0" {
		t.Fatalf("after delete = %+v", e)
	}
	if c := f.do(http.MethodGet, "/repository/"+hostedName+"/web-1.0.0.tgz", nil, "").Code; c != http.StatusNotFound {
		t.Fatalf("deleted archive = %d", c)
	}
	if len(f.env.Events.Published(core.EventComponentDeleted)) != 1 {
		t.Fatal("no component.deleted event")
	}
}

func TestAuthorization(t *testing.T) {
	f := newFixture(t)
	f.mustUpload(hostedName, chartTgz(t, "web", "1.0.0", ""))
	for _, p := range []string{"index.yaml", "web-1.0.0.tgz", "api/charts"} {
		rec := f.anon(http.MethodGet, "/repository/"+hostedName+"/"+p)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Basic") {
			t.Fatalf("anonymous GET %s = %d %v", p, rec.Code, rec.Header())
		}
	}
	read := func(method, path string, body []byte) int {
		req := newRequest(method, path, body)
		req.SetBasicAuth("reader", "pw")
		rec := serve(f.h, req)
		return rec.Code
	}
	if c := read(http.MethodGet, "/repository/"+hostedName+"/index.yaml", nil); c != http.StatusOK {
		t.Fatalf("reader GET index = %d", c)
	}
	if c := read(http.MethodPost, "/repository/"+hostedName+"/api/charts", chartTgz(t, "web", "2.0.0", "")); c != http.StatusForbidden {
		t.Fatalf("reader upload = %d", c)
	}
	if c := read(http.MethodDelete, "/repository/"+hostedName+"/api/charts/web/1.0.0", nil); c != http.StatusForbidden {
		t.Fatalf("reader delete = %d", c)
	}
	// Bearer tokens work too (helm cm-push --access-token).
	req := newRequest(http.MethodGet, "/repository/"+hostedName+"/index.yaml", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	if c := serve(f.h, req).Code; c != http.StatusOK {
		t.Fatalf("bearer = %d", c)
	}
	// Anonymous read when the repository allows it.
	repo, _ := f.env.Repos.Get(context.Background(), hostedName)
	repo.Config.AnonymousRead = true
	f.env.Repos.Put(repo)
	if c := f.anon(http.MethodGet, "/repository/"+hostedName+"/index.yaml").Code; c != http.StatusOK {
		t.Fatalf("anonymous read = %d", c)
	}
}
