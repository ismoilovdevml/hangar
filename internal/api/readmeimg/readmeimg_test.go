package readmeimg_test

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/api/readmeimg"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

var (
	pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)
	svgBytes = []byte(`<?xml version="1.0"?><!-- badge --><!DOCTYPE svg><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
)

func TestSniff(t *testing.T) {
	for _, tc := range []struct {
		body []byte
		want string
	}{
		{pngBytes, readmeimg.TypePNG},
		{[]byte("\xff\xd8\xff\xe0JFIF"), readmeimg.TypeJPEG},
		{[]byte("GIF89a....."), readmeimg.TypeGIF},
		{[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), readmeimg.TypeWebP},
		{[]byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1miaf"), readmeimg.TypeAVIF},
		{[]byte("\x00\x00\x00\x1cftypmif1\x00\x00\x00\x00mif1avifmiaf"), readmeimg.TypeAVIF},
		{svgBytes, readmeimg.TypeSVG},
		{[]byte("\xef\xbb\xbf  <svg width=\"20\">"), readmeimg.TypeSVG},
		{[]byte("<!DOCTYPE svg [<!ENTITY x \"y\">]><svg>"), readmeimg.TypeSVG},
		{[]byte("<html><body><svg></svg></body></html>"), ""},
		{[]byte("<!doctype html><title>404</title>"), ""},
		{[]byte("<svgx>"), ""},
		{[]byte("\x00\x00\x00\x1cftypisom\x00\x00\x00\x00isommp41"), ""},
		{[]byte("GIF8"), ""},
		{[]byte(""), ""},
	} {
		if got := readmeimg.Sniff(tc.body); got != tc.want {
			t.Errorf("Sniff(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestSignatures(t *testing.T) {
	ctx := context.Background()
	a, _ := readmeimg.New(ctx, readmeimg.Options{Key: []byte("0123456789abcdef0123456789abcdef")})
	b, _ := readmeimg.New(ctx, readmeimg.Options{Key: []byte("fedcba9876543210fedcba9876543210")})
	sig := a.Sign("c1", readmeimg.KindURL, "https://img.example.com/a.png")
	if !a.Verify("c1", readmeimg.KindURL, "https://img.example.com/a.png", sig) {
		t.Fatal("own signature rejected")
	}
	for _, bad := range []struct{ id, kind, src, sig string }{
		{"c2", readmeimg.KindURL, "https://img.example.com/a.png", sig},      // other component
		{"c1", readmeimg.KindMember, "https://img.example.com/a.png", sig},   // other kind
		{"c1", readmeimg.KindURL, "https://img.example.com/a.png?x", sig},    // other URL
		{"c1", readmeimg.KindURL, "https://img.example.com/a.png", ""},       // missing
		{"c1", readmeimg.KindURL, "https://img.example.com/a.png", sig[:10]}, // truncated
	} {
		if a.Verify(bad.id, bad.kind, bad.src, bad.sig) {
			t.Errorf("accepted %+v", bad)
		}
	}
	if b.Verify("c1", readmeimg.KindURL, "https://img.example.com/a.png", sig) {
		t.Error("signature valid under another key")
	}
	k1, _ := readmeimg.DeriveKey([]byte("0123456789abcdef0123456789abcdef"))
	k2, _ := readmeimg.DeriveKey([]byte("0123456789abcdef0123456789abcdef"))
	if len(k1) != 32 || !bytes.Equal(k1, k2) {
		t.Errorf("DeriveKey not deterministic: %x %x", k1, k2)
	}
	if _, err := readmeimg.ParseMode("sometimes"); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("ParseMode accepted a bad mode: %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	ctx := context.Background()
	s, _ := readmeimg.New(ctx, readmeimg.Options{})
	h, _ := readmeimg.New(ctx, readmeimg.Options{AllowHTTP: true})
	for in, want := range map[string]string{
		"https://img.example.com/a.svg?label=x#frag": "https://img.example.com/a.svg?label=x",
		"http://img.example.com/a.svg":               "https://img.example.com/a.svg",
		"http://img.example.com:80/a.svg":            "https://img.example.com/a.svg",
		"HTTPS://img.example.com/a.svg":              "https://img.example.com/a.svg",
		"https://user:pw@img.example.com/a.svg":      "",
		"ftp://img.example.com/a.svg":                "",
		"data:image/png;base64,AAAA":                 "",
		"/relative.png":                              "",
	} {
		if got, ok := s.NormalizeURL(in); got != want || ok != (want != "") {
			t.Errorf("NormalizeURL(%s) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if got, _ := h.NormalizeURL("http://img.example.com/a.svg"); got != "http://img.example.com/a.svg" {
		t.Errorf("allow_http NormalizeURL = %s", got)
	}
}

// fixture is a TLS image host, an outbound client factory trusting it and
// a service fetching through both.
type fixture struct {
	t       *testing.T
	srv     *httptest.Server
	hits    atomic.Int32
	headers atomic.Value // http.Header of the last request
	clients *httpclient.Factory
}

func newFixture(t *testing.T, mux http.Handler) *fixture {
	t.Helper()
	f := &fixture{t: t}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.headers.Store(r.Header.Clone())
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	if f.clients, err = httpclient.New(httpclient.Options{CAFile: ca}); err != nil {
		t.Fatal(err)
	}
	return f
}

// service fetches through the fixture; allow is the SSRF allow-list.
func (f *fixture) service(allow []string, o readmeimg.Options) *readmeimg.Service {
	f.t.Helper()
	guard, err := proxy.NewGuard(allow)
	if err != nil {
		f.t.Fatal(err)
	}
	o.Clients, o.Router, o.Guard = f.clients, f.clients, guard
	if len(o.Key) == 0 && o.Pool == nil {
		o.Key = []byte("k")
	}
	s, err := readmeimg.New(context.Background(), o)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func imageHost() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/logo.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain") // the declared type is ignored
		http.SetCookie(w, &http.Cookie{Name: "tracker", Value: "1"})
		_, _ = w.Write(pngBytes)
	})
	mux.HandleFunc("/badge.svg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(svgBytes)
	})
	mux.HandleFunc("/page.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png") // the declared type is not trusted
		_, _ = w.Write([]byte("<html>not an image</html>"))
	})
	mux.HandleFunc("/huge.png", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(append(pngBytes, make([]byte, 2048)...))
	})
	mux.HandleFunc("/missing.png", http.NotFound)
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
	})
	return mux
}

func TestFetch(t *testing.T) {
	f := newFixture(t, imageHost())
	s := f.service([]string{"127.0.0.1"}, readmeimg.Options{MaxBytes: 1024})
	ctx := context.Background()

	img, err := s.Fetch(ctx, f.srv.URL+"/logo.png")
	if err != nil || img.ContentType != readmeimg.TypePNG || !bytes.Equal(img.Body, pngBytes) {
		t.Fatalf("logo = %+v, %v", img, err)
	}
	h := f.headers.Load().(http.Header)
	if h.Get("Referer") != "" || h.Get("Cookie") != "" || h.Get("Authorization") != "" || !strings.Contains(h.Get("Accept"), "image/") {
		t.Errorf("request headers = %v", h)
	}
	// SVG keeps its type (the API serves it under a sandbox policy).
	if img, err := s.Fetch(ctx, f.srv.URL+"/badge.svg"); err != nil || img.ContentType != readmeimg.TypeSVG {
		t.Errorf("badge = %+v, %v", img, err)
	}
	for path, want := range map[string]error{
		"/page.png":    readmeimg.ErrNotImage,
		"/huge.png":    readmeimg.ErrTooLarge,
		"/missing.png": core.ErrUpstream,
	} {
		if _, err := s.Fetch(ctx, f.srv.URL+path); !errors.Is(err, want) && !strings.Contains(err.Error(), want.Error()) {
			t.Errorf("%s: %v, want %v", path, err, want)
		}
	}
}

func TestFetchCachesResultsAndFailures(t *testing.T) {
	f := newFixture(t, imageHost())
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	s := f.service([]string{"127.0.0.1"}, readmeimg.Options{Now: clock})
	ctx := context.Background()
	for range 3 {
		if _, err := s.Fetch(ctx, f.srv.URL+"/logo.png"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Fetch(ctx, f.srv.URL+"/missing.png"); err == nil {
			t.Fatal("missing image served")
		}
	}
	if n := f.hits.Load(); n != 2 {
		t.Fatalf("remote hits = %d, want 2 (one per URL)", n)
	}
	// Failures expire sooner than images.
	now = now.Add(readmeimg.DefaultFailureTTL + time.Second)
	_, _ = s.Fetch(ctx, f.srv.URL+"/logo.png")
	_, _ = s.Fetch(ctx, f.srv.URL+"/missing.png")
	if n := f.hits.Load(); n != 3 {
		t.Fatalf("remote hits after the failure TTL = %d, want 3", n)
	}
	now = now.Add(readmeimg.DefaultTTL)
	_, _ = s.Fetch(ctx, f.srv.URL+"/logo.png")
	if n := f.hits.Load(); n != 4 {
		t.Fatalf("remote hits after the TTL = %d, want 4", n)
	}
}

// The database cache is shared: another node (a fresh service) serves the
// image without fetching it, and accepts URLs the first one signed.
func TestFetchDatabaseCacheAndKey(t *testing.T) {
	pool := apitest.MigratedPool(t)
	f := newFixture(t, imageHost())
	a := f.service([]string{"127.0.0.1"}, readmeimg.Options{Pool: pool})
	b := f.service([]string{"127.0.0.1"}, readmeimg.Options{Pool: pool})
	ctx := context.Background()
	if _, err := a.Fetch(ctx, f.srv.URL+"/logo.png"); err != nil {
		t.Fatal(err)
	}
	img, err := b.Fetch(ctx, f.srv.URL+"/logo.png")
	if err != nil || !bytes.Equal(img.Body, pngBytes) || f.hits.Load() != 1 {
		t.Fatalf("second node: %+v, %v, hits %d", img, err, f.hits.Load())
	}
	if _, err := a.Fetch(ctx, f.srv.URL+"/missing.png"); err == nil {
		t.Fatal("missing served")
	}
	if _, err := b.Fetch(ctx, f.srv.URL+"/missing.png"); err == nil || f.hits.Load() != 2 {
		t.Fatalf("failure not shared: %v, hits %d", err, f.hits.Load())
	}
	sig := a.Sign("c", readmeimg.KindMember, "icon.png")
	if !b.Verify("c", readmeimg.KindMember, "icon.png", sig) {
		t.Error("nodes do not share the signing key")
	}
}

func TestFetchAddressPolicy(t *testing.T) {
	f := newFixture(t, imageHost())
	ctx := context.Background()
	// Loopback is refused unless allow-listed, by literal and by name.
	strict := f.service(nil, readmeimg.Options{})
	for _, u := range []string{f.srv.URL + "/logo.png", strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1) + "/logo.png",
		"https://169.254.169.254/latest/meta-data", "https://[::1]/x.png", "https://10.0.0.1/x.png"} {
		if _, err := strict.Fetch(ctx, u); !errors.Is(err, proxy.ErrBlocked) && !strings.Contains(err.Error(), proxy.ErrBlocked.Error()) {
			t.Errorf("%s: %v, want blocked", u, err)
		}
	}
	if f.hits.Load() != 0 {
		t.Fatalf("blocked requests reached the server: %d", f.hits.Load())
	}
	// Every redirect hop is checked again.
	s := f.service([]string{"127.0.0.1"}, readmeimg.Options{})
	for to, want := range map[string]string{
		"https://10.1.2.3/x.png":        proxy.ErrBlocked.Error(),
		"https://169.254.169.254/x.png": proxy.ErrBlocked.Error(),
		"http://img.example.com/x.png":  readmeimg.ErrNotAllowed.Error(),
		"file:///etc/passwd":            readmeimg.ErrNotAllowed.Error(),
		"/loop":                         "stopped after 5 redirects",
	} {
		_, err := s.Fetch(ctx, f.srv.URL+"/hop?to="+to)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("redirect to %s: %v, want %q", to, err, want)
		}
	}
	// A redirect within the policy is followed.
	if img, err := s.Fetch(ctx, f.srv.URL+"/hop?to=/logo.png"); err != nil || img.ContentType != readmeimg.TypePNG {
		t.Errorf("allowed redirect: %+v, %v", img, err)
	}
	// Plain http only when configured.
	plain := httptest.NewServer(imageHost())
	defer plain.Close()
	if _, err := s.Fetch(ctx, plain.URL+"/logo.png"); err == nil || !strings.Contains(err.Error(), readmeimg.ErrNotAllowed.Error()) {
		t.Errorf("http fetched without allow_http: %v", err)
	}
	h := f.service([]string{"127.0.0.1"}, readmeimg.Options{AllowHTTP: true})
	if img, err := h.Fetch(ctx, plain.URL+"/logo.png"); err != nil || img.ContentType != readmeimg.TypePNG {
		t.Errorf("http with allow_http: %+v, %v", img, err)
	}
}
