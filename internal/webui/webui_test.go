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
		"lake-dawn.jpg":          {Data: []byte("\xff\xd8\xff wallpaper")},
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
		"/favicon.svg":            "no-cache",
		"/lake-dawn.jpg":          "no-cache",
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

// Unhashed files (the login wallpaper) are revalidated instead of being
// downloaded again: every response carries a strong ETag of the bytes sent,
// and a request presenting it is answered 304 without a body.
func TestSPAEntityTags(t *testing.T) {
	h := spa(testBuild())
	conditional := func(p, accept, ifNoneMatch string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Accept-Encoding", accept)
		req.Header.Set("If-None-Match", ifNoneMatch)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	tests := []struct {
		name, path, accept string
	}{
		{"wallpaper", "/lake-dawn.jpg", "gzip, br"},
		{"icon", "/favicon.svg", ""},
		{"index identity", "/", ""},
		{"index brotli", "/", "br"},
		{"spa route gzip", "/repositories", "gzip"},
		{"hashed asset brotli", "/assets/app-1a2b.js", "br"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first := get(t, h, tc.path, tc.accept)
			tag := first.Header().Get("ETag")
			if first.Code != http.StatusOK || len(tag) != 34 || !strings.HasPrefix(tag, `"`) {
				t.Fatalf("GET %s = %d, ETag %q; want 200 with a strong 128-bit tag", tc.path, first.Code, tag)
			}
			for _, inm := range []string{tag, "W/" + tag, `"other", ` + tag, "*"} {
				rec := conditional(tc.path, tc.accept, inm)
				if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != tag {
					t.Errorf("If-None-Match %s = %d, %d bytes, ETag %q; want 304 without body", inm, rec.Code, rec.Body.Len(),
						rec.Header().Get("ETag"))
				}
			}
			if rec := conditional(tc.path, tc.accept, `"stale"`); rec.Code != http.StatusOK || rec.Body.String() != first.Body.String() {
				t.Errorf("stale tag = %d %q; want the full response", rec.Code, rec.Body.String())
			}
		})
	}

	// Each content coding is its own representation with its own tag.
	br := get(t, h, "/", "br").Header().Get("ETag")
	gz := get(t, h, "/", "gzip").Header().Get("ETag")
	id := get(t, h, "/", "").Header().Get("ETag")
	if br == gz || br == id || gz == id {
		t.Fatalf("ETags per encoding: br %s, gzip %s, identity %s; want three different tags", br, gz, id)
	}
	if rec := conditional("/", "gzip", br); rec.Code != http.StatusOK || rec.Body.String() != "index-gz" {
		t.Fatalf("brotli tag on a gzip request = %d %q; want the gzip body", rec.Code, rec.Body.String())
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
