package pypi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

const pipAccept = "application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html; q=0.1, text/html; q=0.01"

type projectPage struct {
	Meta     map[string]string `json:"meta"`
	Name     string            `json:"name"`
	Versions []string          `json:"versions"`
	Files    []struct {
		Filename       string            `json:"filename"`
		URL            string            `json:"url"`
		Hashes         map[string]string `json:"hashes"`
		RequiresPython string            `json:"requires-python"`
		CoreMetadata   any               `json:"core-metadata"`
		DistInfo       any               `json:"dist-info-metadata"`
		Yanked         any               `json:"yanked"`
		Size           *int64            `json:"size"`
		UploadTime     string            `json:"upload-time"`
	} `json:"files"`
}

func decodePage(t *testing.T, rec *httptest.ResponseRecorder) projectPage {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/vnd.pypi.simple.v1+json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	var p projectPage
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHostedUploadAndSimpleAPI(t *testing.T) {
	f := newFixture(t)
	whl := buildWheel(t, "Tiny.Pkg", "0.1.0", ">=3.8")
	sdist := fakeSdist("Tiny.Pkg", "0.1.0")
	f.upload(t, "/repository/pypi-hosted/", "Tiny.Pkg", "0.1.0", whl)
	f.upload(t, "/repository/pypi-hosted/legacy/", "Tiny.Pkg", "0.1.0", sdist)

	// component metadata from the form
	c, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "tiny-pkg", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := c.Attributes["metadata"].(map[string]any)
	if c.Attributes["display_name"] != "Tiny.Pkg" || c.Attributes["summary"] != "test package" || meta["metadata_version"] != "2.1" {
		t.Errorf("component attributes = %v", c.Attributes)
	}
	if cl, _ := meta["classifiers"].([]string); len(cl) != 2 {
		t.Errorf("classifiers = %#v", meta["classifiers"])
	}
	if n := len(f.env.Audit.Events()); n == 0 {
		t.Error("no audit event for upload")
	}

	// JSON project page
	p := decodePage(t, f.get(t, "/repository/pypi-hosted/simple/tiny-pkg/", pipAccept, "bob", "secret"))
	if p.Meta["api-version"] != "1.1" || p.Name != "tiny-pkg" || len(p.Files) != 2 || len(p.Versions) != 1 || p.Versions[0] != "0.1.0" {
		t.Fatalf("page = %+v", p)
	}
	byName := map[string]int{}
	for i, fl := range p.Files {
		byName[fl.Filename] = i
	}
	w := p.Files[byName[whl.filename]]
	if w.URL != "http://hangar.test/repository/pypi-hosted/packages/tiny-pkg/"+whl.filename || w.Hashes["sha256"] != whl.sha256() ||
		w.RequiresPython != ">=3.8" || w.Size == nil || *w.Size != int64(len(whl.data)) || !strings.HasSuffix(w.UploadTime, "Z") {
		t.Errorf("wheel entry = %+v", w)
	}
	cm, _ := w.CoreMetadata.(map[string]any)
	wantMeta := sha256Hex([]byte(wheelMetadata("Tiny.Pkg", "0.1.0", ">=3.8")))
	if cm["sha256"] != wantMeta || w.DistInfo == nil {
		t.Errorf("core-metadata = %v / %v, want sha256 %s", w.CoreMetadata, w.DistInfo, wantMeta)
	}
	if s := p.Files[byName[sdist.filename]]; s.CoreMetadata != nil || s.Yanked != nil {
		t.Errorf("sdist entry = %+v", s)
	}

	// HTML project page
	rec := f.get(t, "/repository/pypi-hosted/simple/tiny-pkg/", "text/html", "bob", "secret")
	body := rec.Body.String()
	for _, want := range []string{
		`<meta name="pypi:repository-version" content="1.1">`,
		`href="http://hangar.test/repository/pypi-hosted/packages/tiny-pkg/` + whl.filename + `#sha256=` + whl.sha256() + `"`,
		`data-requires-python="&gt;=3.8"`,
		`data-core-metadata="sha256=` + wantMeta + `"`,
		`data-dist-info-metadata="sha256=` + wantMeta + `"`,
		`>` + sdist.filename + `</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("HTML page lacks %s\n%s", want, body)
		}
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" || rec.Header().Get("Vary") != "Accept" {
		t.Errorf("HTML headers = %v", rec.Header())
	}

	// project list
	rec = f.get(t, "/repository/pypi-hosted/simple/", "", "bob", "secret")
	if !strings.Contains(rec.Body.String(), `<a href="http://hangar.test/repository/pypi-hosted/simple/tiny-pkg/">tiny-pkg</a>`) {
		t.Errorf("index HTML = %s", rec.Body.String())
	}
	rec = f.get(t, "/repository/pypi-hosted/simple/", "application/vnd.pypi.simple.v1+json", "bob", "secret")
	if !strings.Contains(rec.Body.String(), `"projects":[{"name":"tiny-pkg"}]`) {
		t.Errorf("index JSON = %s", rec.Body.String())
	}

	// downloads: file, metadata, range, HEAD
	rec = f.get(t, "/repository/pypi-hosted/packages/tiny-pkg/"+whl.filename, "", "bob", "secret")
	if rec.Code != 200 || rec.Body.String() != string(whl.data) || rec.Header().Get("ETag") != `"`+whl.sha256()+`"` {
		t.Errorf("wheel download = %d %v", rec.Code, rec.Header())
	}
	rec = f.get(t, "/repository/pypi-hosted/packages/tiny-pkg/"+whl.filename+".metadata", "", "bob", "secret")
	if rec.Code != 200 || rec.Body.String() != wheelMetadata("Tiny.Pkg", "0.1.0", ">=3.8") {
		t.Errorf("metadata download = %d %q", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/repository/pypi-hosted/packages/tiny-pkg/"+sdist.filename, nil)
	req.SetBasicAuth("bob", "secret")
	req.Header.Set("Range", "bytes=0-4")
	if rec := f.do(t, req); rec.Code != http.StatusPartialContent || rec.Body.String() != "sdist" {
		t.Errorf("range = %d %q", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodHead, "/repository/pypi-hosted/packages/tiny-pkg/"+sdist.filename, nil)
	req.SetBasicAuth("bob", "secret")
	if rec := f.do(t, req); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD = %d", rec.Code)
	}
	if got := f.env.Components.Downloads(); got != 2 {
		t.Errorf("RecordDownload calls = %d, want 2 (file GETs only)", got)
	}
}

func TestSimpleAPIRedirectsAndErrors(t *testing.T) {
	f := newFixture(t)
	f.upload(t, "/repository/pypi-hosted/", "tiny-pkg", "0.1.0", fakeSdist("tiny-pkg", "0.1.0"))
	tests := []struct {
		name, path, accept string
		auth               []string
		code               int
		location           string
	}{
		{"non-normalised redirects", "/repository/pypi-hosted/simple/Tiny_Pkg/?format=text/html", "", []string{"bob", "secret"}, 301, "http://hangar.test/repository/pypi-hosted/simple/tiny-pkg/?format=text/html"},
		{"missing slash", "/repository/pypi-hosted/simple/tiny-pkg", "", []string{"bob", "secret"}, 301, "/repository/pypi-hosted/simple/tiny-pkg/"},
		{"missing slash on index", "/repository/pypi-hosted/simple", "", []string{"bob", "secret"}, 301, "/repository/pypi-hosted/simple/"},
		{"unknown project", "/repository/pypi-hosted/simple/nope/", "", []string{"bob", "secret"}, 404, ""},
		{"not acceptable", "/repository/pypi-hosted/simple/tiny-pkg/", "application/vnd.pypi.simple.v2+json", []string{"bob", "secret"}, 406, ""},
		{"anonymous challenged", "/repository/pypi-hosted/simple/tiny-pkg/", "", nil, 401, ""},
		{"bad password", "/repository/pypi-hosted/simple/tiny-pkg/", "", []string{"bob", "wrong"}, 401, ""},
		{"unknown file", "/repository/pypi-hosted/packages/tiny-pkg/tiny_pkg-9.tar.gz", "", []string{"bob", "secret"}, 404, ""},
		{"non-normalised file path", "/repository/pypi-hosted/packages/Tiny-Pkg/tiny_pkg-0.1.0.tar.gz", "", []string{"bob", "secret"}, 404, ""},
		{"traversal", "/repository/pypi-hosted/packages/tiny-pkg/..%2f..%2fx", "", []string{"bob", "secret"}, 404, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.get(t, tt.path, tt.accept, tt.auth...)
			if rec.Code != tt.code {
				t.Fatalf("code = %d, want %d: %s", rec.Code, tt.code, rec.Body.String())
			}
			if tt.location != "" && rec.Header().Get("Location") != tt.location {
				t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), tt.location)
			}
			if tt.code == 401 && rec.Header().Get("WWW-Authenticate") != `Basic realm="hangar"` {
				t.Errorf("challenge = %q", rec.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestAnonymousRead(t *testing.T) {
	f := newFixture(t)
	f.upload(t, "/repository/pypi-hosted/", "tiny-pkg", "0.1.0", fakeSdist("tiny-pkg", "0.1.0"))
	f.hosted.Config.AnonymousRead = true
	f.env.Repos.Put(f.hosted)
	if rec := f.get(t, "/repository/pypi-hosted/simple/tiny-pkg/", ""); rec.Code != 200 {
		t.Fatalf("anonymous read = %d", rec.Code)
	}
	if rec := f.do(t, uploadRequest(t, "/repository/pypi-hosted/", "tiny-pkg", "0.2.0", fakeSdist("tiny-pkg", "0.2.0"), nil, "", "")); rec.Code != 401 {
		t.Fatalf("anonymous upload = %d", rec.Code)
	}
}

func TestUploadValidation(t *testing.T) {
	whl := buildWheel(t, "tiny-pkg", "0.1.0", "")
	sd := fakeSdist("tiny-pkg", "0.1.0")
	tests := []struct {
		name    string
		version string
		d       dist
		extra   map[string]string
		auth    []string
		code    int
		message string
	}{
		{"read-only user", "0.1.0", sd, nil, []string{"bob", "secret"}, 403, "access denied"},
		{"wrong password", "0.1.0", sd, nil, []string{"alice", "nope"}, 401, ""},
		{"sha256 mismatch", "0.1.0", sd, map[string]string{"sha256_digest": strings.Repeat("0", 64)}, nil, 400, "sha256_digest"},
		{"md5 mismatch", "0.1.0", sd, map[string]string{"md5_digest": strings.Repeat("0", 32)}, nil, 400, "md5_digest"},
		{"blake2 mismatch", "0.1.0", sd, map[string]string{"blake2_256_digest": strings.Repeat("0", 64)}, nil, 400, "blake2_256_digest"},
		{"no digest", "0.1.0", sd, map[string]string{"sha256_digest": "", "md5_digest": ""}, nil, 400, "required"},
		{"wrong action", "0.1.0", sd, map[string]string{":action": "submit"}, nil, 400, ":action"},
		{"bad protocol", "0.1.0", sd, map[string]string{"protocol_version": "2"}, nil, 400, "protocol_version"},
		{"bad name", "0.1.0", sd, map[string]string{"name": "bad name"}, nil, 400, "project name"},
		{"foreign filename", "0.1.0", sd, map[string]string{"name": "other"}, nil, 400, "does not start with project"},
		{"version mismatch", "0.2.0", sd, nil, nil, 400, "does not match version"},
		// Regression: "1.0" and "10" compared equal once separators were dropped.
		{"version 1.0 vs file 10", "1.0", fakeSdist("tiny-pkg", "10"), nil, nil, 400, "does not match version"},
		{"PEP 440 spelling of the file version", "0.1", sd, nil, nil, 200, ""},
		{"filetype mismatch", "0.1.0", sd, map[string]string{"filetype": "bdist_wheel"}, nil, 400, "is not a bdist_wheel"},
		{"unsupported filetype", "0.1.0", sd, map[string]string{"filetype": "bdist_egg"}, nil, 400, "unsupported filetype"},
		{"wheel without metadata", "0.1.0", dist{filename: whl.filename, data: []byte("not a zip"), filetype: "bdist_wheel", pyver: "py3"}, nil, nil, 400, "not a zip"},
		{"base64 md5 accepted", "0.1.0", sd, map[string]string{"sha256_digest": "", "md5_digest": "PLACEHOLDER"}, nil, 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.extra["md5_digest"] == "PLACEHOLDER" {
				tt.extra["md5_digest"] = md5B64(tt.d.data)
			}
			name := "tiny-pkg"
			if n, ok := tt.extra["name"]; ok {
				name = n
			}
			rec := f.do(t, uploadRequest(t, "/repository/pypi-hosted/", name, tt.version, tt.d, tt.extra, tt.auth...))
			if rec.Code != tt.code || !strings.Contains(rec.Body.String(), tt.message) {
				t.Fatalf("code = %d %q, want %d %q", rec.Code, rec.Body.String(), tt.code, tt.message)
			}
		})
	}
}

func TestUploadNotMultipart(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/repository/pypi-hosted/", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("alice", "secret")
	if rec := f.do(t, req); rec.Code != 400 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestWritePolicy(t *testing.T) {
	f := newFixture(t)
	f.hosted.Config.Hosted = &core.HostedConfig{WritePolicy: core.WriteAllowOnce}
	f.env.Repos.Put(f.hosted)
	whl := buildWheel(t, "tiny-pkg", "0.1.0", "")
	f.upload(t, "/repository/pypi-hosted/", "tiny-pkg", "0.1.0", whl)
	// identical bytes again: idempotent (twine --skip-existing friendly)
	f.upload(t, "/repository/pypi-hosted/", "tiny-pkg", "0.1.0", whl)
	other := buildWheel(t, "tiny-pkg", "0.1.0", ">=3.12")
	if rec := f.do(t, uploadRequest(t, "/repository/pypi-hosted/", "tiny-pkg", "0.1.0", other, nil)); rec.Code != http.StatusConflict {
		t.Fatalf("overwrite under allow_once = %d %s", rec.Code, rec.Body.String())
	}
	f.hosted.Config.Hosted.WritePolicy = core.WriteDeny
	f.env.Repos.Put(f.hosted)
	if rec := f.do(t, uploadRequest(t, "/repository/pypi-hosted/", "tiny-pkg", "0.2.0", fakeSdist("tiny-pkg", "0.2.0"), nil)); rec.Code != http.StatusConflict {
		t.Fatalf("upload under deny = %d", rec.Code)
	}
}

func TestYank(t *testing.T) {
	f := newFixture(t)
	f.upload(t, "/repository/pypi-hosted/", "tiny-pkg", "0.1.0", fakeSdist("tiny-pkg", "0.1.0"))
	f.upload(t, "/repository/pypi-hosted/", "tiny-pkg", "0.2.0", fakeSdist("tiny-pkg", "0.2.0"))

	yank := func(method, version, body string, auth ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/repository/pypi-hosted/yank/Tiny_Pkg/"+version, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(auth[0], auth[1])
		return f.do(t, req)
	}
	if rec := yank(http.MethodPost, "0.1.0", `{"reason":"broken build"}`, "bob", "secret"); rec.Code != 403 {
		t.Fatalf("yank by reader = %d", rec.Code)
	}
	if rec := yank(http.MethodPost, "9.9", ``, "alice", "secret"); rec.Code != 404 {
		t.Fatalf("yank unknown = %d", rec.Code)
	}
	if rec := yank(http.MethodPost, "0.1.0", `{"reason":"broken build"}`, "alice", "secret"); rec.Code != 200 {
		t.Fatalf("yank = %d %s", rec.Code, rec.Body.String())
	}
	if rec := yank(http.MethodPost, "0.2.0", ``, "alice", "secret"); rec.Code != 200 {
		t.Fatalf("yank without reason = %d %s", rec.Code, rec.Body.String())
	}
	p := decodePage(t, f.get(t, "/repository/pypi-hosted/simple/tiny-pkg/", pipAccept, "bob", "secret"))
	if p.Files[0].Yanked != "broken build" || p.Files[1].Yanked != true {
		t.Fatalf("yanked = %v / %v", p.Files[0].Yanked, p.Files[1].Yanked)
	}
	html := f.get(t, "/repository/pypi-hosted/simple/tiny-pkg/", "text/html", "bob", "secret").Body.String()
	if !strings.Contains(html, `data-yanked="broken build"`) || !strings.Contains(html, `data-yanked=""`) {
		t.Errorf("HTML yanked = %s", html)
	}
	if rec := yank(http.MethodDelete, "0.1.0", ``, "alice", "secret"); rec.Code != 200 {
		t.Fatalf("unyank = %d", rec.Code)
	}
	p = decodePage(t, f.get(t, "/repository/pypi-hosted/simple/tiny-pkg/", pipAccept, "bob", "secret"))
	if p.Files[0].Yanked != nil {
		t.Errorf("still yanked: %v", p.Files[0].Yanked)
	}
	var yankAudits int
	for _, e := range f.env.Audit.Events() {
		if e.Action == "component.yank" || e.Action == "component.unyank" {
			yankAudits++
		}
	}
	if yankAudits != 3 {
		t.Errorf("yank audit events = %d, want 3", yankAudits)
	}
}

func TestQuarantinedReleaseHidden(t *testing.T) {
	var q *quarantining
	f := newFixture(t, func(env *coretest.Env, d *core.Deps) {
		q = &quarantining{Components: env.Components, ids: map[string]bool{}}
		d.Components = q
	})
	sd := fakeSdist("tiny-pkg", "0.1.0")
	f.upload(t, "/repository/pypi-hosted/", "tiny-pkg", "0.1.0", sd)
	f.upload(t, "/repository/pypi-hosted/", "tiny-pkg", "0.2.0", fakeSdist("tiny-pkg", "0.2.0"))
	c, _ := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "tiny-pkg", "0.1.0")
	q.set(c.ID)
	p := decodePage(t, f.get(t, "/repository/pypi-hosted/simple/tiny-pkg/", pipAccept, "bob", "secret"))
	if len(p.Files) != 1 || p.Files[0].Filename != "tiny_pkg-0.2.0.tar.gz" {
		t.Errorf("files = %+v", p.Files)
	}
	if rec := f.get(t, "/repository/pypi-hosted/packages/tiny-pkg/"+sd.filename, "", "bob", "secret"); rec.Code != 403 {
		t.Errorf("quarantined download = %d", rec.Code)
	}
}

// Regression: Simple API pages had no validator, so pip and uv downloaded
// every project page again on each resolve. Pages carry an ETag per
// rendering and a matching If-None-Match answers 304 until the project
// changes.
func TestSimplePagesETag(t *testing.T) {
	f := newFixture(t)
	f.upload(t, "/repository/pypi-hosted/", "etag-pkg", "1.0.0", fakeSdist("etag-pkg", "1.0.0"))
	get := func(path, accept, inm string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Header.Set("Accept", accept)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		req.SetBasicAuth("bob", "secret")
		return f.do(t, req)
	}
	for _, path := range []string{"/repository/pypi-hosted/simple/etag-pkg/", "/repository/pypi-hosted/simple/"} {
		jsonPage := get(path, pipAccept, "")
		htmlPage := get(path, "text/html", "")
		etag := jsonPage.Header().Get("ETag")
		if jsonPage.Code != http.StatusOK || etag == "" || htmlPage.Header().Get("ETag") == "" || htmlPage.Header().Get("ETag") == etag {
			t.Fatalf("%s: json %d ETag %q, html ETag %q (want distinct validators)", path, jsonPage.Code, etag, htmlPage.Header().Get("ETag"))
		}
		if rec := get(path, pipAccept, etag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
			t.Fatalf("%s: revalidation = %d with %d bytes, want 304", path, rec.Code, rec.Body.Len())
		}
		if rec := get(path, "text/html", etag); rec.Code != http.StatusOK {
			t.Fatalf("%s: other rendering with the JSON ETag = %d, want 200", path, rec.Code)
		}
	}
	// A new release changes the page and its validator.
	page := "/repository/pypi-hosted/simple/etag-pkg/"
	before := get(page, pipAccept, "").Header().Get("ETag")
	f.upload(t, "/repository/pypi-hosted/", "etag-pkg", "1.1.0", fakeSdist("etag-pkg", "1.1.0"))
	if rec := get(page, pipAccept, before); rec.Code != http.StatusOK || rec.Header().Get("ETag") == before {
		t.Fatalf("after a new release: %d ETag %q (was %q)", rec.Code, rec.Header().Get("ETag"), before)
	}
}
