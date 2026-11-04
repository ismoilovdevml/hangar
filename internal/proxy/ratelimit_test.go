package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
)

func TestRetryAfterWithinBudgetIsHonoured(t *testing.T) {
	var hits atomic.Int32
	var gap atomic.Int64
	var last atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := time.Now().UnixNano()
		if p := last.Swap(now); p != 0 {
			gap.Store(now - p)
		}
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	m := newManager(t, nil)
	u := upstreamFor(t, m, proxyRepo("rl-ok", srv.URL, nil))
	before := testutil.ToFloat64(retriesTotal.WithLabelValues("rl-ok", retryReasonRateLimit))
	if _, b := fetchOK(t, u, core.UpstreamRequest{Path: "x"}); b != "ok" {
		t.Fatalf("body %q", b)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", hits.Load())
	}
	if g := time.Duration(gap.Load()); g < 900*time.Millisecond {
		t.Errorf("retried after %s, want the remote's Retry-After of 1s", g)
	}
	if d := testutil.ToFloat64(retriesTotal.WithLabelValues("rl-ok", retryReasonRateLimit)) - before; d != 1 {
		t.Errorf("rate-limit retries metric += %v, want 1", d)
	}
}

func TestRateLimitBeyondBudgetSurfacesAs429(t *testing.T) {
	var hits atomic.Int32
	var limited atomic.Bool
	limited.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if limited.Load() {
			w.Header().Set("Retry-After", "120")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	clock := newClock()
	bus := coretest.NewEventBus()
	m := newManager(t, func(o *Options) { o.Now = clock.Now; o.Events = bus })
	repo := proxyRepo("rl", srv.URL, func(c *core.ProxyConfig) { c.CircuitFailures = 2 })
	u := upstreamFor(t, m, repo)

	_, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "x"})
	if !errors.Is(err, core.ErrRateLimited) || !errors.Is(err, core.ErrUpstream) || core.HTTPStatus(err) != http.StatusTooManyRequests {
		t.Fatalf("want a rate-limit error answered 429, got %v (status %d)", err, core.HTTPStatus(err))
	}
	if d, ok := core.RetryAfter(err); !ok || d != 120*time.Second {
		t.Fatalf("RetryAfter = %s %v, want 120s", d, ok)
	}
	h := http.Header{}
	core.SetRetryAfter(h, err)
	if h.Get("Retry-After") != "120" {
		t.Errorf("Retry-After header = %q", h.Get("Retry-After"))
	}
	if hits.Load() != 1 {
		t.Fatalf("a Retry-After beyond the budget must not be waited for: hits=%d", hits.Load())
	}

	// The host is gated: further requests fail fast with the remaining wait,
	// and many 429s never open the circuit.
	clock.Advance(20 * time.Second)
	for range 5 {
		_, err = u.Fetch(context.Background(), core.UpstreamRequest{Path: "y"})
		if !errors.Is(err, core.ErrRateLimited) {
			t.Fatalf("gated: want ErrRateLimited, got %v", err)
		}
	}
	if d, _ := core.RetryAfter(err); d != 100*time.Second {
		t.Errorf("gated RetryAfter = %s, want 100s", d)
	}
	if hits.Load() != 1 {
		t.Fatalf("gated host contacted: hits=%d", hits.Load())
	}
	if st := m.Health(repo).State; st != StateSlow {
		t.Errorf("rate-limited state = %s, want slow", st)
	}

	// After the gate the remote is asked again; repeated 429s still do not
	// trip the breaker.
	clock.Advance(101 * time.Second)
	for range 3 {
		_, err = u.Fetch(context.Background(), core.UpstreamRequest{Path: "z"})
		if !errors.Is(err, core.ErrRateLimited) {
			t.Fatalf("want ErrRateLimited, got %v", err)
		}
		clock.Advance(121 * time.Second)
	}
	if st := u.breaker.State().String(); st != "closed" {
		t.Fatalf("breaker = %s after 429s, want closed", st)
	}

	// An administrator lifts the gate early.
	if _, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "w"}); !errors.Is(err, core.ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	limited.Store(false)
	res, err := m.InvalidateCache(context.Background(), repo)
	if err != nil || res.RateLimitedHosts != 1 {
		t.Fatalf("InvalidateCache = %+v, %v", res, err)
	}
	if _, b := fetchOK(t, u, core.UpstreamRequest{Path: "after"}); b != "ok" {
		t.Fatalf("after invalidation: %q", b)
	}
	if evs := bus.Published(core.EventRepositoryCacheInvalidated); len(evs) != 1 || evs[0].Repository != "rl" {
		t.Errorf("events = %+v", evs)
	}
}

func TestRetryAfter503AndHTTPDate(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		v    string
		want time.Duration
		ok   bool
	}{
		{"0", 0, true},
		{"7", 7 * time.Second, true},
		{" 30 ", 30 * time.Second, true},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"-1", 0, false},
		{"soon", 0, false},
		{"", 0, false},
	} {
		h := http.Header{}
		if tt.v != "" {
			h.Set("Retry-After", tt.v)
		}
		if got, ok := retryAfter(h, now); got != tt.want || ok != tt.ok {
			t.Errorf("retryAfter(%q) = %s %v, want %s %v", tt.v, got, ok, tt.want, tt.ok)
		}
	}

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "maintenance", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "back")
	}))
	defer srv.Close()
	m := newManager(t, func(o *Options) { o.RetryBackoff = time.Hour }) // only Retry-After can make this fast
	u := upstreamFor(t, m, proxyRepo("p", srv.URL, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := u.Fetch(ctx, core.UpstreamRequest{Path: "x"})
	if err != nil {
		t.Fatalf("503 with Retry-After: 0 must be retried at once: %v", err)
	}
	if b := body(t, resp); b != "back" {
		t.Fatalf("body %q", b)
	}
}

func TestOfflineMode(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	m := newManager(t, nil)
	repo := proxyRepo("air-gapped", srv.URL, func(c *core.ProxyConfig) { c.Offline = true })
	u := upstreamFor(t, m, repo)
	_, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "lodash"})
	if !errors.Is(err, ErrOfflineMode) || !errors.Is(err, core.ErrUpstream) || !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("offline: got %v", err)
	}
	if core.HTTPStatus(err) != http.StatusNotFound {
		t.Errorf("offline cache miss answers %d, want 404", core.HTTPStatus(err))
	}
	if !strings.Contains(err.Error(), "offline mode") {
		t.Errorf("message %q", err)
	}
	if hits.Load() != 0 {
		t.Fatal("offline repository contacted its remote")
	}
	if st := m.Health(repo).State; st != StateOffline {
		t.Errorf("state = %s, want offline", st)
	}
	// Switching offline mode off takes effect on the next lookup.
	repo.Config.Proxy.Offline = false
	if _, err := upstreamFor(t, m, repo).Fetch(context.Background(), core.UpstreamRequest{Path: "lodash"}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d", hits.Load())
	}
}

func TestInvalidateCacheAcrossNodes(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	bus := coretest.NewEventBus()
	node1 := newManager(t, func(o *Options) { o.Events = bus })
	node2 := newManager(t, func(o *Options) { o.Events = bus })
	sub, err := node2.Watch(context.Background(), bus)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	repo := proxyRepo("maven-central", srv.URL, nil)
	u1, u2 := upstreamFor(t, node1, repo), upstreamFor(t, node2, repo)
	for range 2 {
		fetchOK(t, u1, core.UpstreamRequest{Path: "com/acme/x/1.0/x-1.0.pom"})
		fetchOK(t, u2, core.UpstreamRequest{Path: "com/acme/x/1.0/x-1.0.pom"})
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2 (one per node)", hits.Load())
	}
	res, err := node1.InvalidateCache(context.Background(), repo)
	if err != nil || res.NegativeEntries != 1 || res.Repository != "maven-central" {
		t.Fatalf("InvalidateCache = %+v, %v", res, err)
	}
	waitFor(t, "node2 purge", func() bool {
		u2.neg.mu.Lock()
		defer u2.neg.mu.Unlock()
		return len(u2.neg.entries) == 0
	})
	fetchOK(t, u1, core.UpstreamRequest{Path: "com/acme/x/1.0/x-1.0.pom"})
	fetchOK(t, u2, core.UpstreamRequest{Path: "com/acme/x/1.0/x-1.0.pom"})
	if hits.Load() != 4 {
		t.Fatalf("hits after invalidation = %d, want 4", hits.Load())
	}
	if _, err := node1.InvalidateCache(context.Background(), &core.Repository{Name: "h", Type: core.RepoHosted}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("hosted: want ErrInvalid, got %v", err)
	}
}
