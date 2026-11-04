package pypi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

// remote is a fake index: project "foo" in JSON (files on the remote itself
// and on files.example), "bar" in HTML only, everything else 404.
type remote struct {
	t       *testing.T
	wheel   dist
	sdist   dist
	meta    string
	down    atomic.Bool
	files   atomic.Int32 // file requests served
	pages   atomic.Int32 // project pages served with 200
	revalid atomic.Int32 // 304 answers
}

func newRemote(t *testing.T) *remote {
	return &remote{t: t, wheel: buildWheel(t, "foo", "1.0", ">=3.9"), sdist: fakeSdist("foo", "1.0"), meta: wheelMetadata("foo", "1.0", ">=3.9")}
}

func (rm *remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if rm.down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case "/simple/foo/":
		if r.Header.Get("If-None-Match") == `"v1"` {
			rm.revalid.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if !strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json") {
			rm.t.Errorf("index request Accept = %q", r.Header.Get("Accept"))
		}
		rm.pages.Add(1)
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		w.Header().Set("ETag", `"v1"`)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"meta": map[string]string{"api-version": "1.1"}, "name": "foo", "versions": []string{"0.8", "0.9", "1.0"},
			"files": []map[string]any{
				{"filename": rm.wheel.filename, "url": "/files/" + rm.wheel.filename, "hashes": map[string]string{"sha256": rm.wheel.sha256()},
					"requires-python": ">=3.9", "core-metadata": map[string]string{"sha256": sha256Hex([]byte(rm.meta))}, "size": len(rm.wheel.data),
					"upload-time": "2025-01-02T03:04:05.000000Z"},
				{"filename": rm.sdist.filename, "url": "https://files.example/pkgs/" + rm.sdist.filename, "hashes": map[string]string{"sha256": rm.sdist.sha256()},
					"yanked": "use 1.1", "size": len(rm.sdist.data)},
				{"filename": "foo-0.9.tar.gz", "url": "/files/foo-0.9.tar.gz", "hashes": map[string]string{"sha256": strings.Repeat("ab", 32)}, "size": 3},
				{"filename": "foo-0.8.tar.gz", "url": "https://evil.example/foo-0.8.tar.gz", "hashes": map[string]string{}, "size": 3},
			},
		})
	case "/simple/bar/":
		rm.pages.Add(1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><a href="/files/bar-2.0.tar.gz#sha256=%s" data-requires-python="&gt;=3.10">bar-2.0.tar.gz</a></body></html>`, sha256Hex([]byte("bar")))
	case "/files/" + rm.wheel.filename:
		rm.files.Add(1)
		_, _ = w.Write(rm.wheel.data)
	case "/files/" + rm.wheel.filename + ".metadata":
		rm.files.Add(1)
		_, _ = w.Write([]byte(rm.meta))
	case "/files/foo-0.9.tar.gz", "/pkgs/foo-0.9.tar.gz":
		rm.files.Add(1)
		_, _ = w.Write([]byte("tampered"))
	case "/pkgs/" + rm.sdist.filename:
		rm.files.Add(1)
		_, _ = w.Write(rm.sdist.data)
	case "/files/bar-2.0.tar.gz":
		rm.files.Add(1)
		_, _ = w.Write([]byte("bar"))
	default:
		http.NotFound(w, r)
	}
}

func newProxyFixture(t *testing.T, ttl time.Duration) (*fixture, *remote, *core.Repository) {
	t.Helper()
	rm := newRemote(t)
	f := newFixture(t, func(env *coretest.Env, d *core.Deps) {
		d.HTTPClients = handlerClients{hosts: map[string]http.Handler{"files.example": rm, "evil.example": rm}}
	})
	proxy := f.env.AddRepo(t, &core.Repository{Name: "pypi-proxy", Format: "pypi", Type: core.RepoProxy, Config: core.RepoConfig{
		Proxy:  &core.ProxyConfig{RemoteURL: "https://pypi.example", MetadataTTL: core.Duration(ttl)},
		Format: map[string]any{"file_hosts": []any{"files.example"}},
	}})
	f.env.Upstreams.Set("pypi-proxy", rm)
	return f, rm, proxy
}

func TestProxyProjectPage(t *testing.T) {
	f, rm, _ := newProxyFixture(t, time.Hour)
	p := decodePage(t, f.get(t, "/repository/pypi-proxy/simple/foo/", pipAccept, "bob", "secret"))
	if len(p.Files) != 4 || p.Meta["api-version"] != "1.1" {
		t.Fatalf("page = %+v", p)
	}
	byName := map[string]int{}
	for i, fl := range p.Files {
		byName[fl.Filename] = i
	}
	w := p.Files[byName[rm.wheel.filename]]
	if w.URL != "http://hangar.test/repository/pypi-proxy/packages/foo/"+rm.wheel.filename || w.Hashes["sha256"] != rm.wheel.sha256() ||
		w.RequiresPython != ">=3.9" || w.UploadTime != "2025-01-02T03:04:05.000000Z" {
		t.Errorf("wheel = %+v", w)
	}
	if cm, _ := w.CoreMetadata.(map[string]any); cm["sha256"] != sha256Hex([]byte(rm.meta)) {
		t.Errorf("core-metadata = %v", w.CoreMetadata)
	}
	if s := p.Files[byName[rm.sdist.filename]]; s.Yanked != "use 1.1" || !strings.HasPrefix(s.URL, "http://hangar.test/repository/pypi-proxy/") {
		t.Errorf("sdist = %+v", s)
	}
	// cached within the TTL
	f.get(t, "/repository/pypi-proxy/simple/foo/", "text/html", "bob", "secret")
	if n := rm.pages.Load(); n != 1 {
		t.Errorf("remote page fetches = %d, want 1", n)
	}
	// the cached projects make up the proxy's project list
	if body := f.get(t, "/repository/pypi-proxy/simple/", pipAccept, "bob", "secret").Body.String(); !strings.Contains(body, `{"name":"foo"}`) {
		t.Errorf("proxy index = %s", body)
	}

	// HTML-only remote
	rec := f.get(t, "/repository/pypi-proxy/simple/bar/", pipAccept, "bob", "secret")
	p = decodePage(t, rec)
	if len(p.Files) != 1 || p.Files[0].RequiresPython != ">=3.10" || p.Meta["api-version"] != "1.0" || p.Files[0].Size != nil {
		t.Errorf("bar = %s", rec.Body.String())
	}
	if rec := f.get(t, "/repository/pypi-proxy/packages/bar/bar-2.0.tar.gz", "", "bob", "secret"); rec.Code != 200 || rec.Body.String() != "bar" {
		t.Errorf("bar download = %d %s", rec.Code, rec.Body.String())
	}
}

func TestProxyFiles(t *testing.T) {
	f, rm, proxy := newProxyFixture(t, time.Hour)
	get := func(path string) (int, string) {
		rec := f.get(t, "/repository/pypi-proxy/packages/foo/"+path, "", "bob", "secret")
		return rec.Code, rec.Body.String()
	}
	if code, body := get(rm.wheel.filename); code != 200 || body != string(rm.wheel.data) {
		t.Fatalf("wheel = %d", code)
	}
	if code, body := get(rm.wheel.filename + ".metadata"); code != 200 || body != rm.meta {
		t.Fatalf("metadata = %d %q", code, body)
	}
	if code, body := get(rm.sdist.filename); code != 200 || body != string(rm.sdist.data) {
		t.Fatalf("file from files host = %d %q", code, body)
	}
	served := rm.files.Load()
	for range 3 {
		if code, _ := get(rm.wheel.filename); code != 200 {
			t.Fatal(code)
		}
	}
	if rm.files.Load() != served {
		t.Errorf("cached file fetched again: %d -> %d", served, rm.files.Load())
	}
	// cached as a component; the cache fill is attributed to the reader
	c, err := f.env.Components.GetComponent(t.Context(), proxy.ID, "", "foo", "1.0")
	if err != nil || c.PublishedBy != "bob" {
		t.Errorf("cached component = %+v, %v", c, err)
	}

	tests := []struct {
		file string
		code int
	}{
		{"foo-0.9.tar.gz", 502},              // hash mismatch
		{"foo-0.8.tar.gz", 502},              // host not in file_hosts
		{"foo-0.7.tar.gz", 404},              // not listed
		{"foo-0.9.tar.gz.metadata", 404},     // no metadata advertised
		{"foo-1.0.tar.gz.metadata", 404},     // sdist without metadata
		{"../../simple/foo/index.json", 404}, // never reachable as a file
		{"foo-0.9.tar.gz", 502},              // mismatch is not cached
	}
	for _, tt := range tests {
		if code, body := get(tt.file); code != tt.code {
			t.Errorf("GET %s = %d %s, want %d", tt.file, code, body, tt.code)
		}
	}
	if _, err := f.env.Components.GetAsset(t.Context(), proxy.ID, "packages/foo/foo-0.9.tar.gz"); err == nil {
		t.Error("file with mismatching hash was cached")
	}
	// the remote's 404 is remembered
	for range 2 {
		if rec := f.get(t, "/repository/pypi-proxy/simple/missing/", "", "bob", "secret"); rec.Code != 404 {
			t.Fatalf("missing = %d", rec.Code)
		}
	}
	before := f.env.Upstreams.Calls("pypi-proxy")
	f.get(t, "/repository/pypi-proxy/simple/missing/", "", "bob", "secret")
	if f.env.Upstreams.Calls("pypi-proxy") != before {
		t.Error("negative cache not used")
	}
	// proxies are read-only
	if rec := f.do(t, uploadRequest(t, "/repository/pypi-proxy/", "foo", "1.0", rm.sdist, nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("upload to proxy = %d", rec.Code)
	}
}

func TestProxyRevalidationAndStale(t *testing.T) {
	f, rm, _ := newProxyFixture(t, time.Nanosecond)
	page := func() projectPage {
		return decodePage(t, f.get(t, "/repository/pypi-proxy/simple/foo/", pipAccept, "bob", "secret"))
	}
	page()
	page() // expired: conditional request, 304
	if rm.pages.Load() != 1 || rm.revalid.Load() != 1 {
		t.Errorf("pages = %d, 304s = %d", rm.pages.Load(), rm.revalid.Load())
	}
	rm.down.Store(true)
	if p := page(); len(p.Files) != 4 {
		t.Errorf("stale page = %+v", p)
	}
	// a project never cached cannot be served while the remote is down
	if rec := f.get(t, "/repository/pypi-proxy/simple/bar/", "", "bob", "secret"); rec.Code != http.StatusBadGateway {
		t.Errorf("uncached while down = %d", rec.Code)
	}
}

func TestGroup(t *testing.T) {
	f, rm, _ := newProxyFixture(t, time.Hour)
	f.env.AddRepo(t, &core.Repository{Name: "pypi-group", Format: "pypi", Type: core.RepoGroup, Config: core.RepoConfig{
		Group: &core.GroupConfig{Members: []string{"pypi-hosted", "pypi-proxy"}, WriteMember: "pypi-hosted"},
	}})
	f.env.AddRepo(t, &core.Repository{Name: "pypi-group-ro", Format: "pypi", Type: core.RepoGroup, Config: core.RepoConfig{
		Group: &core.GroupConfig{Members: []string{"pypi-group"}},
	}})
	// the hosted member has its own 2.0 and a different build of 1.0's sdist: it wins
	mine := dist{filename: rm.sdist.filename, data: []byte("internal build"), filetype: "sdist", pyver: "source"}
	f.upload(t, "/repository/pypi-hosted/", "foo", "1.0", mine)
	// uploads to the group land in its write member
	f.upload(t, "/repository/pypi-group/", "foo", "2.0", fakeSdist("foo", "2.0"))
	if _, err := f.env.Components.GetComponent(t.Context(), f.hosted.ID, "", "foo", "2.0"); err != nil {
		t.Fatalf("group upload not in hosted member: %v", err)
	}
	if rec := f.do(t, uploadRequest(t, "/repository/pypi-group-ro/", "foo", "3.0", fakeSdist("foo", "3.0"), nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("upload to group without write member = %d", rec.Code)
	}

	for _, group := range []string{"pypi-group", "pypi-group-ro"} {
		p := decodePage(t, f.get(t, "/repository/"+group+"/simple/foo/", pipAccept, "bob", "secret"))
		byName := map[string]int{}
		for i, fl := range p.Files {
			byName[fl.Filename] = i
			if !strings.HasPrefix(fl.URL, "http://hangar.test/repository/"+group+"/packages/foo/") {
				t.Errorf("%s link %s", group, fl.URL)
			}
		}
		if len(p.Files) != 5 {
			t.Fatalf("%s files = %+v", group, p.Files)
		}
		if s := p.Files[byName[rm.sdist.filename]]; s.Hashes["sha256"] != sha256Hex(mine.data) || s.Yanked != nil {
			t.Errorf("%s: hosted file should win: %+v", group, s)
		}
		for file, want := range map[string]string{
			rm.sdist.filename:               "internal build",
			"foo-2.0.tar.gz":                "sdist foo 2.0",
			rm.wheel.filename:               string(rm.wheel.data),
			rm.wheel.filename + ".metadata": rm.meta,
		} {
			if rec := f.get(t, "/repository/"+group+"/packages/foo/"+file, "", "bob", "secret"); rec.Code != 200 || rec.Body.String() != want {
				t.Errorf("%s %s = %d", group, file, rec.Code)
			}
		}
		if rec := f.get(t, "/repository/"+group+"/packages/foo/foo-0.9.tar.gz", "", "bob", "secret"); rec.Code != http.StatusBadGateway {
			t.Errorf("%s bad member file = %d", group, rec.Code)
		}
		body := f.get(t, "/repository/"+group+"/simple/", pipAccept, "bob", "secret").Body.String()
		if !strings.Contains(body, `{"name":"foo"}`) {
			t.Errorf("%s index = %s", group, body)
		}
	}
	if rec := f.get(t, "/repository/pypi-group/simple/nothing/", "", "bob", "secret"); rec.Code != 404 {
		t.Errorf("unknown project in group = %d", rec.Code)
	}
}
