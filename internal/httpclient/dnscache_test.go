package httpclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// fakeResolver answers from a map, or with err; it counts queries and can
// hold them until release is closed.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][]netip.Addr
	err     error
	queries atomic.Int32
	release chan struct{}
}

func (r *fakeResolver) LookupNetIP(ctx context.Context, _, host string) ([]netip.Addr, error) {
	r.queries.Add(1)
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	if a, ok := r.answers[host]; ok {
		return append([]netip.Addr(nil), a...), nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (r *fakeResolver) set(host string, addrs ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.answers == nil {
		r.answers = map[string][]netip.Addr{}
	}
	r.answers[host] = nil
	for _, a := range addrs {
		r.answers[host] = append(r.answers[host], netip.MustParseAddr(a))
	}
}

func (r *fakeResolver) fail(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var errDNSTimeout = &net.DNSError{Err: "i/o timeout", Name: "registry.example.com", IsTimeout: true, IsTemporary: true}

func newTestCache(r Resolver, clk *testClock, opts DNSOptions) *DNSCache {
	opts.now, opts.backoff = clk.now, time.Millisecond
	return NewDNSCache(r, opts)
}

func lookup(t *testing.T, c *DNSCache, host string) ([]netip.Addr, error) {
	t.Helper()
	return c.LookupNetIP(context.Background(), "ip", host)
}

func TestDNSCacheHitMissTTL(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)}
	r := &fakeResolver{}
	r.set("registry.example.com", "203.0.114.10", "203.0.114.11")
	c := newTestCache(r, clk, DNSOptions{TTL: time.Minute})
	hits := testutil.ToFloat64(dnsCacheTotal.WithLabelValues(dnsResultHit))
	misses := testutil.ToFloat64(dnsCacheTotal.WithLabelValues(dnsResultMiss))

	for range 3 {
		a, err := lookup(t, c, "Registry.Example.com.")
		if err != nil || len(a) != 2 || a[0].String() != "203.0.114.10" {
			t.Fatalf("lookup = %v, %v", a, err)
		}
		a[0] = netip.MustParseAddr("10.0.0.1") // the caller's copy
	}
	if n := r.queries.Load(); n != 1 {
		t.Errorf("queries within TTL = %d, want 1", n)
	}
	if d := testutil.ToFloat64(dnsCacheTotal.WithLabelValues(dnsResultHit)) - hits; d != 2 {
		t.Errorf("hits = %v, want 2", d)
	}
	if d := testutil.ToFloat64(dnsCacheTotal.WithLabelValues(dnsResultMiss)) - misses; d != 1 {
		t.Errorf("misses = %v, want 1", d)
	}
	clk.advance(time.Minute + time.Second)
	r.set("registry.example.com", "203.0.114.12")
	a, err := lookup(t, c, "registry.example.com")
	if err != nil || len(a) != 1 || a[0].String() != "203.0.114.12" || r.queries.Load() != 2 {
		t.Errorf("after TTL = %v, %v (queries %d)", a, err, r.queries.Load())
	}
	// IP literals are not looked up.
	if a, err := lookup(t, c, "198.51.100.7"); err != nil || a[0].String() != "198.51.100.7" || r.queries.Load() != 2 {
		t.Errorf("literal = %v, %v", a, err)
	}
}

func TestDNSCacheBounds(t *testing.T) {
	c := NewDNSCache(nil, DNSOptions{})
	if c.ttl != DefaultDNSTTL || c.neg != DefaultDNSNegativeTTL || c.stale != DefaultDNSStaleTTL {
		t.Errorf("defaults = %v %v %v", c.ttl, c.neg, c.stale)
	}
	c = NewDNSCache(nil, DNSOptions{TTL: time.Millisecond, NegativeTTL: -1, StaleTTL: -1})
	if c.ttl != MinDNSTTL || c.neg != 0 || c.stale != 0 {
		t.Errorf("low = %v %v %v", c.ttl, c.neg, c.stale)
	}
	c = NewDNSCache(nil, DNSOptions{TTL: 48 * time.Hour, NegativeTTL: time.Hour, StaleTTL: 1000 * time.Hour})
	if c.ttl != MaxDNSTTL || c.neg != MaxDNSNegativeTTL || c.stale != MaxDNSStaleTTL {
		t.Errorf("high = %v %v %v", c.ttl, c.neg, c.stale)
	}
}

func TestDNSCacheStaleWhileError(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)}
	r := &fakeResolver{}
	r.set("registry.example.com", "203.0.114.10")
	c := newTestCache(r, clk, DNSOptions{TTL: time.Minute, NegativeTTL: 5 * time.Second, StaleTTL: 10 * time.Minute})
	if _, err := lookup(t, c, "registry.example.com"); err != nil {
		t.Fatal(err)
	}
	stale := testutil.ToFloat64(dnsCacheTotal.WithLabelValues(dnsResultStale))
	timeouts := testutil.ToFloat64(dnsErrorsTotal.WithLabelValues("timeout"))

	// The DNS servers time out: the last answer is served, after one retry.
	r.fail(errDNSTimeout)
	clk.advance(2 * time.Minute)
	a, err := lookup(t, c, "registry.example.com")
	if err != nil || len(a) != 1 || a[0].String() != "203.0.114.10" {
		t.Fatalf("stale lookup = %v, %v", a, err)
	}
	if n := r.queries.Load(); n != 3 {
		t.Errorf("queries = %d, want 1 + 2 (one retry)", n)
	}
	if d := testutil.ToFloat64(dnsCacheTotal.WithLabelValues(dnsResultStale)) - stale; d != 1 {
		t.Errorf("stale = %v", d)
	}
	if d := testutil.ToFloat64(dnsErrorsTotal.WithLabelValues("timeout")) - timeouts; d != 2 {
		t.Errorf("timeout errors = %v, want 2", d)
	}
	// Within the negative TTL the stale answer is served without asking.
	if _, err := lookup(t, c, "registry.example.com"); err != nil || r.queries.Load() != 3 {
		t.Errorf("within negative TTL: %v, queries %d", err, r.queries.Load())
	}
	// Stale answers are bounded by StaleTTL.
	clk.advance(9 * time.Minute)
	if _, err := lookup(t, c, "registry.example.com"); err == nil || !IsTransientDNSError(err) {
		t.Errorf("past stale TTL = %v, want the DNS error", err)
	}
	// The failure is remembered for the negative TTL.
	q := r.queries.Load()
	if _, err := lookup(t, c, "registry.example.com"); err == nil || r.queries.Load() != q {
		t.Errorf("negative entry: %v, queries %d -> %d", err, q, r.queries.Load())
	}
	clk.advance(6 * time.Second)
	r.fail(nil)
	if _, err := lookup(t, c, "registry.example.com"); err != nil {
		t.Errorf("recovered: %v", err)
	}
}

func TestDNSCacheNXDomainIsFinal(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)}
	r := &fakeResolver{}
	r.set("registry.example.com", "203.0.114.10")
	c := newTestCache(r, clk, DNSOptions{TTL: time.Minute, NegativeTTL: 5 * time.Second})
	if _, err := lookup(t, c, "registry.example.com"); err != nil {
		t.Fatal(err)
	}
	// The name is removed: no stale answer, no retry.
	r.set("registry.example.com")
	r.mu.Lock()
	delete(r.answers, "registry.example.com")
	r.mu.Unlock()
	clk.advance(2 * time.Minute)
	_, err := lookup(t, c, "registry.example.com")
	if !dnsNotFound(err) || IsTransientDNSError(err) {
		t.Fatalf("nxdomain = %v", err)
	}
	if n := r.queries.Load(); n != 2 {
		t.Errorf("queries = %d, want 2 (no retry of NXDOMAIN)", n)
	}
	if _, err := lookup(t, c, "registry.example.com"); !dnsNotFound(err) || r.queries.Load() != 2 {
		t.Errorf("cached nxdomain = %v, queries %d", err, r.queries.Load())
	}
}

func TestDNSCacheSingleflight(t *testing.T) {
	clk := &testClock{t: time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)}
	r := &fakeResolver{release: make(chan struct{})}
	r.set("registry.example.com", "203.0.114.10")
	c := newTestCache(r, clk, DNSOptions{})

	const callers = 50
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			a, err := lookup(t, c, "registry.example.com")
			if err == nil && (len(a) != 1 || a[0].String() != "203.0.114.10") {
				err = errors.New("wrong answer")
			}
			errs <- err
		})
	}
	// A caller that gives up does not cancel the shared lookup.
	ctx, cancel := context.WithCancel(context.Background())
	quit := make(chan error, 1)
	go func() {
		_, err := c.LookupNetIP(ctx, "ip", "registry.example.com")
		quit <- err
	}()
	cancel()
	if err := <-quit; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled caller = %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	close(r.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := r.queries.Load(); n != 1 {
		t.Errorf("queries for %d concurrent lookups = %d, want 1", callers, n)
	}
}

// TestDialThroughDNSCacheKeepsGuard: connections resolved from the cache
// are still checked by DialGuard, address by address.
func TestDialThroughDNSCacheKeepsGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	r := &fakeResolver{}
	r.set("mirror.example.com", "127.0.0.1")
	r.set("rebind.example.com", "127.0.0.1")
	cache := NewDNSCache(r, DNSOptions{})
	var dialled []netip.Addr
	var mu sync.Mutex
	f, err := New(Options{environ: env(nil), DNS: cache,
		DialGuard: func(a netip.Addr) error {
			mu.Lock()
			dialled = append(dialled, a)
			mu.Unlock()
			return errors.New("blocked by outbound address policy")
		},
		DialAllowHost: func(h string) bool { return h == "mirror.example.com" }})
	if err != nil {
		t.Fatal(err)
	}
	get := func(host string) error {
		u := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/"
		c, err := f.ForUpstream(u, core.UpstreamHTTP{}, nil)
		if err != nil {
			return err
		}
		resp, err := c.Get(u)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
	// Allow-listed host: dialled through the cache without the guard.
	if err := get("mirror.example.com"); err != nil {
		t.Fatalf("allowed host: %v", err)
	}
	// A cached private answer is refused at dial time.
	if err := get("rebind.example.com"); err == nil {
		t.Fatal("dial to a cached loopback answer was allowed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dialled) != 1 || dialled[0].String() != "127.0.0.1" {
		t.Errorf("guard saw %v", dialled)
	}
	if n := r.queries.Load(); n != 2 {
		t.Errorf("queries = %d", n)
	}
}

func TestDialCachedFallsBackToOtherFamily(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	r := &fakeResolver{}
	// Nothing listens on the IPv6 loopback: the IPv4 fallback answers.
	r.set("dual.example.com", "::1", "127.0.0.1")
	c, err := dialCached(context.Background(), &net.Dialer{}, NewDNSCache(r, DNSOptions{}), 5*time.Second, "tcp",
		net.JoinHostPort("dual.example.com", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.RemoteAddr().(*net.TCPAddr).IP.String(); got != "127.0.0.1" {
		t.Errorf("connected to %s", got)
	}
	// A failed lookup is a dial error that keeps the DNS error.
	r.fail(errDNSTimeout)
	_, err = dialCached(context.Background(), &net.Dialer{}, NewDNSCache(r, DNSOptions{backoff: time.Millisecond}), time.Second, "tcp", "down.example.com:443")
	var op *net.OpError
	if !errors.As(err, &op) || !IsTransientDNSError(err) {
		t.Errorf("lookup failure = %v", err)
	}
}
