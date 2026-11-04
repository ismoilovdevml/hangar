package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats/builtin"
	"github.com/ismoilovdevml/hangar/internal/health"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func securedRouter(t *testing.T, sec Security) http.Handler {
	t.Helper()
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: "raw-hosted", Format: "raw", Type: core.RepoHosted})
	trusted, _ := ParseTrustedProxies([]string{"10.0.0.0/8"})
	deps := env.Deps()
	hc := health.New(time.Second)
	return NewRouter(RouterOptions{
		Logger: discard(), Health: hc, TrustedProxies: trusted,
		API:     &api.API{Deps: deps, Health: hc, Formats: reg.Names(), Started: time.Now()},
		Formats: reg, Deps: deps, Security: sec,
	})
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestSecurityHeaders(t *testing.T) {
	h := securedRouter(t, Security{HSTSMaxAge: 24 * time.Hour, BaseURL: "https://hangar.example"})
	for path, csp := range map[string]string{
		"/":                               CSPWebUI,
		"/repositories":                   CSPWebUI,
		"/api/v1/system/status":           CSPAPI,
		"/repository/raw-hosted/file.txt": CSPPackage,
		"/v2/":                            CSPPackage,
	} {
		rec := serve(h, httptest.NewRequest(http.MethodGet, path, nil))
		hdr := rec.Header()
		if hdr.Get("Content-Security-Policy") != csp {
			t.Errorf("%s: CSP = %q", path, hdr.Get("Content-Security-Policy"))
		}
		if !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: framing allowed", path)
		}
		if hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Referrer-Policy") != "no-referrer" || hdr.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: headers = %v", path, hdr)
		}
		if hdr.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS on plain HTTP", path)
		}
	}
	if cc := serve(h, httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)).Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("API Cache-Control = %q", cc)
	}

	// HTTPS: TLS on the connection, or X-Forwarded-Proto from a trusted proxy.
	r := httptest.NewRequest(http.MethodGet, "https://hangar.example/", nil)
	if got := serve(h, r).Header().Get("Strict-Transport-Security"); got != "max-age=86400" {
		t.Errorf("HSTS over TLS = %q", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := serve(h, r).Header().Get("Strict-Transport-Security"); got != "max-age=86400" {
		t.Errorf("HSTS behind trusted proxy = %q", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("X-Forwarded-Proto", "https") // untrusted peer
	if got := serve(h, r).Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS from spoofed header = %q", got)
	}
}

func TestCORS(t *testing.T) {
	h := securedRouter(t, Security{BaseURL: "https://hangar.example", AllowedOrigins: []string{"https://portal.example"}})
	preflight := func(origin, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodOptions, path, nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "PUT")
		r.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
		return serve(h, r)
	}
	if rec := preflight("https://evil.example", "/api/v1/repositories"); rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("foreign preflight = %d %v", rec.Code, rec.Header())
	}
	if rec := preflight("https://evil.example", "/repository/raw-hosted/x"); rec.Code != http.StatusForbidden {
		t.Errorf("foreign preflight to package endpoint = %d", rec.Code)
	}
	rec := preflight("https://portal.example", "/api/v1/repositories")
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://portal.example" ||
		rec.Header().Get("Access-Control-Allow-Credentials") != "true" || rec.Header().Get("Access-Control-Allow-Headers") != "authorization, content-type" {
		t.Errorf("allowed preflight = %d %v", rec.Code, rec.Header())
	}

	req := func(method, origin, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return serve(h, r)
	}
	for _, c := range []struct {
		method, origin, path string
		code                 int
	}{
		{http.MethodGet, "https://evil.example", "/api/v1/system/status", http.StatusForbidden},
		{http.MethodGet, "null", "/api/v1/system/status", http.StatusForbidden},
		{http.MethodPost, "https://evil.example", "/repository/raw-hosted/x", http.StatusForbidden},
		{http.MethodGet, "https://evil.example", "/healthz", http.StatusOK}, // readable only without CORS headers
		{http.MethodGet, "https://hangar.example", "/api/v1/system/status", http.StatusOK},
		{http.MethodGet, "http://example.com", "/api/v1/system/status", http.StatusOK}, // the request's own origin (httptest Host)
		{http.MethodGet, "https://portal.example", "/api/v1/system/status", http.StatusOK},
		{http.MethodGet, "", "/api/v1/system/status", http.StatusOK},
	} {
		rec := req(c.method, c.origin, c.path)
		if rec.Code != c.code {
			t.Errorf("%s %s from %q = %d, want %d", c.method, c.path, c.origin, rec.Code, c.code)
		}
		acao := rec.Header().Get("Access-Control-Allow-Origin")
		if (c.origin == "https://portal.example") != (acao != "") {
			t.Errorf("%s from %q: Access-Control-Allow-Origin = %q", c.path, c.origin, acao)
		}
	}
}

func TestBodyLimits(t *testing.T) {
	h := securedRouter(t, Security{APIBodyBytes: 1024, UploadBodyBytes: 2048})
	post := func(path, ct string, n int) int {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", n)))
		r.Header.Set("Content-Type", ct)
		return serve(h, r).Code
	}
	if code := post("/api/v1/repositories", "application/json", 4096); code != http.StatusRequestEntityTooLarge {
		t.Errorf("large API body = %d", code)
	}
	if code := post("/api/v1/repositories", "application/json", 100); code == http.StatusRequestEntityTooLarge {
		t.Errorf("small API body = %d", code)
	}
	if code := post("/repository/raw-hosted/f.bin", "application/octet-stream", 4096); code != http.StatusRequestEntityTooLarge {
		t.Errorf("upload over upload limit = %d", code)
	}
	if code := post("/", "text/plain", maxOtherBody+1); code != http.StatusRequestEntityTooLarge {
		t.Errorf("large UI body = %d", code)
	}
	// Unlimited uploads by default.
	h = securedRouter(t, Security{APIBodyBytes: 1024})
	r := httptest.NewRequest(http.MethodPut, "/repository/raw-hosted/f.bin", strings.NewReader(strings.Repeat("x", 1<<20)))
	if code := serve(h, r).Code; code == http.StatusRequestEntityTooLarge {
		t.Errorf("default upload limit = %d", code)
	}
}

func TestAPIRateLimit(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h := securedRouter(t, Security{APIRate: Rate{PerMinute: 60, Burst: 3}, Now: clock.now})
	get := func(ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/system/version", nil)
		r.RemoteAddr = ip + ":1000"
		return serve(h, r)
	}
	for i := range 3 {
		if code := get("203.0.113.1").Code; code != http.StatusOK {
			t.Fatalf("request %d = %d", i, code)
		}
	}
	rec := get("203.0.113.1")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), `"rate_limited"`) {
		t.Errorf("over limit = %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if code := get("203.0.113.2").Code; code != http.StatusOK {
		t.Errorf("other client = %d", code)
	}
	// Package endpoints are not limited by the API limit.
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = "203.0.113.1:1000"
	if code := serve(h, r).Code; code != http.StatusOK {
		t.Errorf("healthz = %d", code)
	}
	clock.add(time.Second)
	if code := get("203.0.113.1").Code; code != http.StatusOK {
		t.Errorf("after refill = %d", code)
	}
	// IPv6 clients are grouped by /64.
	for range 3 {
		get("2001:db8:1:2::1")
	}
	if code := get("2001:db8:1:2::ffff").Code; code != http.StatusTooManyRequests {
		t.Errorf("same /64 = %d", code)
	}
}

func TestFailedAuthLimit(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	h := securedRouter(t, Security{FailedAuthRate: Rate{PerMinute: 6, Burst: 2}, Now: clock.now})
	get := func(path string, creds bool) int {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "198.51.100.7:1000"
		if creds {
			r.SetBasicAuth("alice", "wrong")
		}
		return serve(h, r).Code
	}
	// Anonymous 401s (the registry challenge) do not count.
	for range 5 {
		if code := get("/v2/", false); code != http.StatusUnauthorized {
			t.Fatalf("anonymous /v2/ = %d", code)
		}
	}
	for i := range 2 {
		if code := get("/repository/raw-hosted/f", true); code != http.StatusUnauthorized {
			t.Fatalf("failed attempt %d = %d", i, code)
		}
	}
	if code := get("/repository/raw-hosted/f", true); code != http.StatusTooManyRequests {
		t.Errorf("after the budget = %d", code)
	}
	if code := get("/api/v1/system/status", true); code != http.StatusTooManyRequests {
		t.Errorf("API with credentials after the budget = %d", code)
	}
	if code := get("/repository/raw-hosted/f", false); code != http.StatusUnauthorized {
		t.Errorf("anonymous request after the budget = %d", code)
	}
	clock.add(10 * time.Second)
	if code := get("/repository/raw-hosted/f", true); code != http.StatusUnauthorized {
		t.Errorf("after refill = %d", code)
	}
}

// A body that stalls is cut off by the idle timeout; one that keeps
// flowing is not, however long it takes in total.
func TestBodyIdleTimeout(t *testing.T) {
	var mu sync.Mutex
	results := map[string]string{}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		mu.Lock()
		if err != nil {
			results[r.URL.Path] = "error"
		} else {
			results[r.URL.Path] = "ok"
		}
		mu.Unlock()
		_ = n
		w.WriteHeader(http.StatusNoContent)
	})
	sec := newHardening(Security{BodyIdleTimeout: 300 * time.Millisecond})
	h := RequestInfo(nil)(sec.bodyLimit(0)(inner))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan net.Addr, 1)
	go func() {
		_ = Run(ctx, Config{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second, ShutdownTimeout: time.Second}, h, nil, discard(), func(a net.Addr) { addrc <- a })
	}()
	addr := (<-addrc).String()

	send := func(path string, pause time.Duration, chunks int) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "POST "+path+" HTTP/1.1\r\nHost: x\r\nContent-Length: "+strconv.Itoa(chunks)+"\r\n\r\n")
		for range chunks {
			time.Sleep(pause)
			if _, err := io.WriteString(conn, "x"); err != nil {
				return
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _ = io.ReadAll(conn)
	}
	send("/steady", 100*time.Millisecond, 8) // 800 ms total, never idle 300 ms
	send("/stalled", time.Second, 2)
	mu.Lock()
	defer mu.Unlock()
	if results["/steady"] != "ok" || results["/stalled"] != "error" {
		t.Errorf("results = %v", results)
	}
}

// README images loaded directly from their hosts (ui.readme_images: direct)
// widen img-src only; nothing else of the web UI's policy changes.
func TestUIImageSources(t *testing.T) {
	h := securedRouter(t, Security{UIImageSources: []string{"https:"}})
	got := serve(h, httptest.NewRequest(http.MethodGet, "/", nil)).Header().Get("Content-Security-Policy")
	if want := strings.Replace(CSPWebUI, "img-src 'self' data:;", "img-src 'self' data: https:;", 1); got != want || got == CSPWebUI {
		t.Errorf("CSP = %q, want %q", got, want)
	}
	if got := serve(h, httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)).Header().Get("Content-Security-Policy"); got != CSPAPI {
		t.Errorf("API CSP = %q", got)
	}
}
