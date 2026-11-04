package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/formats/builtin"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/observability"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestResolveClient(t *testing.T) {
	trusted, err := ParseTrustedProxies([]string{"10.0.0.0/8", "192.168.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name               string
		remote             string
		xff, proto, host   string
		wantIP, wantScheme string
		wantHost           string
	}{
		{"direct client, headers ignored", "203.0.113.7:5000", "1.2.3.4", "https", "evil.example", "203.0.113.7", "http", "hangar.local"},
		{"trusted proxy", "10.1.1.1:5000", "198.51.100.9", "https", "cr.example.com", "198.51.100.9", "https", "cr.example.com"},
		{"chain of trusted proxies", "10.1.1.1:5000", "198.51.100.9, 192.168.1.10, 10.2.2.2", "", "", "198.51.100.9", "http", "hangar.local"},
		{"spoofed left-most entry ignored", "10.1.1.1:5000", "6.6.6.6, 198.51.100.9", "", "", "198.51.100.9", "http", "hangar.local"},
		{"garbage proto ignored", "192.168.1.10:1", "", "gopher", "", "192.168.1.10", "http", "hangar.local"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://hangar.local/x", nil)
			r.RemoteAddr = tt.remote
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tt.proto)
			}
			if tt.host != "" {
				r.Header.Set("X-Forwarded-Host", tt.host)
			}
			got := resolveClient(r, trusted)
			if got.RemoteIP != tt.wantIP || got.Scheme != tt.wantScheme || got.Host != tt.wantHost {
				t.Errorf("got %+v, want ip=%s scheme=%s host=%s", got, tt.wantIP, tt.wantScheme, tt.wantHost)
			}
		})
	}
	if _, err := ParseTrustedProxies([]string{"nope"}); err == nil {
		t.Error("bad CIDR accepted")
	}
}

func newTestRouter(t *testing.T) (http.Handler, *health.Checker) {
	t.Helper()
	reg, err := builtin.Registry()
	if err != nil {
		t.Fatal(err)
	}
	env := coretest.NewEnv(t)
	env.AddRepo(t, &core.Repository{Name: "raw-hosted", Format: "raw", Type: core.RepoHosted})
	hc := health.New(time.Second)
	hc.Add("database", func(context.Context) error { return nil })
	h := NewRouter(RouterOptions{
		Logger:  discard(),
		Metrics: observability.NewMetrics(),
		Health:  hc,
		Formats: reg,
		Deps:    env.Deps(),
		Started: time.Now(),
	})
	return h, hc
}

func TestRouter(t *testing.T) {
	h, _ := newTestRouter(t)
	do := func(method, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set(HeaderRequestID, "abc-123")
		h.ServeHTTP(rec, req)
		return rec
	}
	tests := []struct {
		path     string
		code     int
		contains string
	}{
		{"/healthz", 200, `"ok"`},
		{"/readyz", 200, `"database"`},
		{"/api/v1/system/status", 200, `"dependencies"`},
		{"/api/v1/formats", 200, `"docker"`},
		{"/api/v1/repositories", 501, `"not_implemented"`},
		{"/repository/raw-hosted/file.txt", 401, "authentication required"},
		{"/repository/absent/file.txt", 404, "not found"},
		{"/v2/", 401, "UNAUTHORIZED"},
		{"/repositories", 200, "<html"},
	}
	for _, tt := range tests {
		rec := do(http.MethodGet, tt.path)
		if rec.Code != tt.code || !strings.Contains(rec.Body.String(), tt.contains) {
			t.Errorf("GET %s = %d %q, want %d containing %q", tt.path, rec.Code, rec.Body.String(), tt.code, tt.contains)
		}
		if rec.Header().Get(HeaderRequestID) != "abc-123" {
			t.Errorf("GET %s: request id not echoed", tt.path)
		}
	}
	rec := do(http.MethodGet, "/metrics")
	for _, want := range []string{"hangar_build_info", `hangar_http_requests_total{code="200",method="GET",route="/healthz"}`, `route="/repository/{repo}/*"`, "go_goroutines"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("/metrics missing %s", want)
		}
	}
	var st struct {
		State      string `json:"state"`
		NodesTotal int    `json:"nodes_total"`
	}
	_ = json.Unmarshal(do(http.MethodGet, "/api/v1/system/status").Body.Bytes(), &st)
	if st.State != "ok" || st.NodesTotal != 1 {
		t.Errorf("status = %+v", st)
	}
}

func TestRecover(t *testing.T) {
	h := Recover(discard())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 500 {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(started)
			<-release
		}
		_, _ = io.WriteString(w, "done")
	})
	hc := health.New(0)
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan net.Addr, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- Run(ctx, Config{Addr: "127.0.0.1:0", ShutdownTimeout: 5 * time.Second}, h, hc, discard(), func(a net.Addr) { addrc <- a })
	}()
	addr := (<-addrc).String()

	respc := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/slow")
		if err != nil {
			respc <- "error: " + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		respc <- string(b)
	}()
	<-started
	cancel()
	time.Sleep(100 * time.Millisecond)
	if rep := hc.Run(context.Background()); rep.Status != health.StatusDraining {
		t.Errorf("readiness during shutdown = %s", rep.Status)
	}
	close(release)
	if got := <-respc; got != "done" {
		t.Fatalf("in-flight request = %q", got)
	}
	if err := <-errc; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if resp, err := http.Get("http://" + addr + "/"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("server still accepting after shutdown")
	}
}
