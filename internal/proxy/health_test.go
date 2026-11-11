package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// TestHealthFromTraffic: a remote nobody used is unknown, not healthy; the
// first answer makes it healthy; a request that fails (5xx or no answer)
// makes it offline at once, with the reason, before the breaker trips; the
// next answer clears it.
func TestHealthFromTraffic(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	m := newManager(t, func(o *Options) { o.Retries = -1 })
	repo := proxyRepo("p", srv.URL, func(c *core.ProxyConfig) { c.CircuitFailures = 5 })

	if it := m.Health(repo); it.State != StateUnknown || it.LastError != "" {
		t.Fatalf("never used: %+v, want unknown", it)
	}
	u := upstreamFor(t, m, repo)
	if st := u.health().State; st != StateUnknown {
		t.Fatalf("created, not used: %s, want unknown", st)
	}

	fetchOK(t, u, core.UpstreamRequest{Path: "a"})
	if it := m.Health(repo); it.State != StateHealthy || it.LastError != "" {
		t.Fatalf("after an answer: %+v", it)
	}

	status.Store(http.StatusBadGateway)
	if resp, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "b"}); err == nil {
		_ = resp.Body.Close()
	}
	it := m.Health(repo)
	if it.State != StateOffline || !strings.Contains(it.LastError, "502") {
		t.Fatalf("after one 502: %+v, want offline naming the 502", it)
	}

	status.Store(http.StatusOK)
	fetchOK(t, u, core.UpstreamRequest{Path: "c"})
	if it := m.Health(repo); it.State != StateHealthy || it.LastError != "" {
		t.Fatalf("after recovering: %+v", it)
	}

	// A remote that cannot be reached at all.
	dead := proxyRepo("dead", "http://127.0.0.1:1/", nil)
	du := upstreamFor(t, m, dead)
	if _, err := du.Fetch(context.Background(), core.UpstreamRequest{Path: "x"}); !errors.Is(err, core.ErrUpstream) {
		t.Fatalf("dead remote: %v", err)
	}
	if it := m.Health(dead); it.State != StateOffline || it.LastError == "" {
		t.Fatalf("dead remote: %+v, want offline with the error", it)
	}
}
