package webui

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandlerServesPage(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/", "/repositories", "/packages/npm-hosted/@acme/ui"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Errorf("GET %s = %d %s", p, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", rec.Code)
	}
}

func testBuild() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><title>Hangar</title>")},
		"index.html.br":          {Data: []byte("index-br")},
		"index.html.gz":          {Data: []byte("index-gz")},
		"assets/app-1a2b.js":     {Data: []byte("console.log('app')")},
		"assets/app-1a2b.js.br":  {Data: []byte("app-br")},
		"assets/app-1a2b.js.gz":  {Data: []byte("app-gz")},
		"assets/font-3c4d.woff2": {Data: []byte("wOF2")},
		"favicon.svg":            {Data: []byte("<svg/>")},
	}
}

func get(t *testing.T, h http.Handler, p, acceptEncoding string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, p, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSPAServesPrecompressedVariants(t *testing.T) {
	h := spa(testBuild())
	for _, tc := range []struct {
		path, accept, encoding, body, contentType string
	}{
		{"/assets/app-1a2b.js", "gzip, deflate, br, zstd", "br", "app-br", "text/javascript"},
		{"/assets/app-1a2b.js", "gzip", "gzip", "app-gz", "text/javascript"},
		{"/assets/app-1a2b.js", "br;q=0.5, gzip", "gzip", "app-gz", "text/javascript"},
		{"/assets/app-1a2b.js", "br;q=0, gzip;q=0", "", "console.log('app')", "text/javascript"},
		{"/assets/app-1a2b.js", "*", "br", "app-br", "text/javascript"},
		{"/assets/app-1a2b.js", "", "", "console.log('app')", "text/javascript"},
		{"/assets/app-1a2b.js", "identity", "", "console.log('app')", "text/javascript"},
		{"/", "gzip, br", "br", "index-br", "text/html"},
		{"/repositories/npm-hosted", "gzip", "gzip", "index-gz", "text/html"},
		{"/index.html", "", "", "<!doctype html><title>Hangar</title>", "text/html"},
	} {
		rec := get(t, h, tc.path, tc.accept)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s (%q) = %d", tc.path, tc.accept, rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != tc.encoding {
			t.Errorf("GET %s (%q): Content-Encoding %q, want %q", tc.path, tc.accept, got, tc.encoding)
		}
		if got := rec.Body.String(); got != tc.body {
			t.Errorf("GET %s (%q): body %q, want %q", tc.path, tc.accept, got, tc.body)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
			t.Errorf("GET %s: Content-Type %q, want %s", tc.path, got, tc.contentType)
		}
		if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Errorf("GET %s: Vary %q", tc.path, got)
		}
		if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(tc.body)) {
			t.Errorf("GET %s: Content-Length %q, want %d", tc.path, got, len(tc.body))
		}
	}
}

func TestSPACacheHeaders(t *testing.T) {
	h := spa(testBuild())
	for p, want := range map[string]string{
		"/assets/app-1a2b.js":     "public, max-age=31536000, immutable",
		"/assets/font-3c4d.woff2": "public, max-age=31536000, immutable",
		"/":                       "no-cache",
		"/index.html":             "no-cache",
		"/browse":                 "no-cache",
		"/favicon.svg":            "",
	} {
		rec := get(t, h, p, "gzip, br")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", p, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != want {
			t.Errorf("GET %s: Cache-Control %q, want %q", p, got, want)
		}
	}
	// A file without variants is sent as is and does not vary.
	rec := get(t, h, "/assets/font-3c4d.woff2", "gzip, br")
	if rec.Header().Get("Content-Encoding") != "" || rec.Header().Get("Vary") != "" || rec.Body.String() != "wOF2" {
		t.Errorf("woff2: %v %q", rec.Header(), rec.Body.String())
	}
}

func TestSPAMissingAssetIsNotFound(t *testing.T) {
	h := spa(testBuild())
	rec := get(t, h, "/assets/app-0000.js", "gzip, br")
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Encoding") != "" {
		t.Fatalf("missing asset = %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/assets/app-1a2b.js", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/assets/app-1a2b.js", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
}
