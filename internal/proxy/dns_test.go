package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
)

// flakyDNS answers from a map until it is told to time out.
type flakyDNS struct {
	mu      sync.Mutex
	answers map[string][]netip.Addr
	down    bool
	queries atomic.Int32
}

func (r *flakyDNS) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.queries.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down {
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true, IsTemporary: true}
	}
	if a, ok := r.answers[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (r *flakyDNS) set(host, addr string) {
	r.mu.Lock()
	r.answers[host] = []netip.Addr{netip.MustParseAddr(addr)}
	r.mu.Unlock()
}

// TestGuardChecksCachedAnswers: answers from the DNS cache are checked like
// fresh ones, so a name whose cached answer is internal stays blocked.
func TestGuardChecksCachedAnswers(t *testing.T) {
	r := &flakyDNS{answers: map[string][]netip.Addr{}}
	r.set("registry.example.com", "104.16.0.35")
	r.set("rebind.example.com", "104.16.0.36")
	r.set("internal.example.com", "10.0.0.5")
	cache := httpclient.NewDNSCache(r, httpclient.DNSOptions{TTL: time.Second})
	g, err := NewGuard(nil)
	if err != nil {
		t.Fatal(err)
	}
	g.UseResolver(cache)
	check := func(host string) error {
		u, _ := url.Parse("https://" + host + "/v3/index.json")
		return g.Check(context.Background(), u, false)
	}
	for range 2 { // the second check is a cache hit
		if err := check("internal.example.com"); !errors.Is(err, ErrBlocked) {
			t.Fatalf("cached private answer: %v", err)
		}
	}
	if err := check("registry.example.com"); err != nil {
		t.Fatalf("public: %v", err)
	}
	// A name that later resolves to an internal address is blocked once the
	// cache refreshes; until then the cached public answer is also what the
	// dialler connects to.
	if err := check("rebind.example.com"); err != nil {
		t.Fatal(err)
	}
	r.set("rebind.example.com", "169.254.169.254")
	time.Sleep(1100 * time.Millisecond)
	if err := check("rebind.example.com"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("rebound to metadata address: %v", err)
	}
	if n := r.queries.Load(); n != 4 {
		t.Errorf("queries = %d, want 4", n)
	}
}

// TestDNSTimeoutBurstDoesNotOpenBreaker: a burst of requests failing on
// the same DNS timeout is one breaker failure; a DNS outage that lasts
// trips the breaker after the usual number of failures; NXDOMAIN counts as
// a failure every time.
func TestDNSTimeoutBurstDoesNotOpenBreaker(t *testing.T) {
	clk := newClock()
	r := &flakyDNS{answers: map[string][]netip.Addr{}, down: true}
	g, err := NewGuard(loopbackAllowed)
	if err != nil {
		t.Fatal(err)
	}
	g.resolver = r
	m := newManager(t, func(o *Options) { o.Guard = g; o.Retries = -1; o.Now = clk.Now })
	repo := proxyRepo("nuget-gitlab", "https://gitlab.example.com/api/v4/projects/7/packages/nuget", func(c *core.ProxyConfig) {
		c.CircuitFailures = 3
		c.CircuitOpenFor = core.Duration(time.Minute)
	})
	u := upstreamFor(t, m, repo)

	fetch := func(path string) error {
		_, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: path})
		return err
	}
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			if err := fetch(fmt.Sprintf("p%d", i)); !errors.Is(err, core.ErrUpstream) {
				t.Errorf("fetch during DNS outage: %v", err)
			}
		})
	}
	wg.Wait()
	// The requests failed, so the remote reads offline; the breaker stays
	// closed and lets the next request try.
	if st := u.breaker.State(); st != gobreaker.StateClosed {
		t.Fatalf("after one DNS timeout burst: breaker %s, want closed", st)
	}
	if it := m.Health(repo); it.State != StateOffline || it.LastError == "" {
		t.Fatalf("after one DNS timeout burst: %+v, want offline with the error", it)
	}
	// The outage goes on: one more counted failure per second.
	clk.Advance(time.Second)
	_ = fetch("a")
	_ = fetch("b") // same second: not counted
	if st := u.breaker.State(); st != gobreaker.StateClosed {
		t.Fatalf("after two counted DNS failures: breaker %s", st)
	}
	clk.Advance(time.Second)
	_ = fetch("c")
	if st := u.breaker.State(); st != gobreaker.StateOpen {
		t.Fatalf("after a lasting DNS outage: breaker %s, want open", st)
	}

	// NXDOMAIN is a real failure: each one counts.
	r.mu.Lock()
	r.down = false
	r.mu.Unlock()
	m2 := newManager(t, func(o *Options) { o.Guard = g; o.Retries = -1; o.Now = clk.Now })
	u2 := upstreamFor(t, m2, proxyRepo("gone", "https://gone.example.com/", func(c *core.ProxyConfig) { c.CircuitFailures = 3 }))
	for i := range 3 {
		if _, err := u2.Fetch(context.Background(), core.UpstreamRequest{Path: fmt.Sprint(i)}); !errors.Is(err, core.ErrUpstream) {
			t.Fatalf("nxdomain fetch: %v", err)
		}
	}
	if st := u2.health().State; st != StateOffline {
		t.Errorf("after 3 NXDOMAIN: state %s, want offline", st)
	}
}

// TestBlockedReason: 401 and 403 are told apart from each other and from
// the address policy, and a later success clears them.
func TestBlockedReason(t *testing.T) {
	var status atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	m := newManager(t, nil)
	repo := proxyRepo("nuget-gitlab", srv.URL, nil)
	u := upstreamFor(t, m, repo)
	for _, tc := range []struct {
		status int
		state  State
		reason Reason
	}{
		{http.StatusUnauthorized, StateBlocked, ReasonAuthFailed},
		{http.StatusForbidden, StateBlocked, ReasonAccessDenied},
		{http.StatusNotFound, StateHealthy, ""},
		{http.StatusUnauthorized, StateBlocked, ReasonAuthFailed},
		{http.StatusOK, StateHealthy, ""},
	} {
		status.Store(int32(tc.status))
		resp, _ := fetchOK(t, u, core.UpstreamRequest{Path: fmt.Sprint("x", tc.status)})
		if resp.StatusCode != tc.status {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if h := m.Health(repo); h.State != tc.state || h.Reason != tc.reason {
			t.Errorf("after %d: %+v, want %s %q", tc.status, h, tc.state, tc.reason)
		}
	}
}
