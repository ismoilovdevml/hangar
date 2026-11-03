package proxy

import (
	"context"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
)

// loopbackAllowed lets tests reach httptest servers on 127.0.0.1.
var loopbackAllowed = []string{"127.0.0.1/32", "::1"}

func newFactory(t *testing.T, opts httpclient.Options) *httpclient.Factory {
	t.Helper()
	f, err := httpclient.New(opts)
	if err != nil {
		t.Fatalf("httpclient.New: %v", err)
	}
	return f
}

// newManager returns a Manager with a direct client, loopback allowed, fast
// retries and the given overrides.
func newManager(t *testing.T, mod func(*Options)) *Manager {
	t.Helper()
	g, err := NewGuard(loopbackAllowed)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{HTTPClients: newFactory(t, httpclient.Options{}), Guard: g, RetryBackoff: time.Millisecond}
	if mod != nil {
		mod(&opts)
	}
	m, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func proxyRepo(name, remote string, mod func(*core.ProxyConfig)) *core.Repository {
	cfg := &core.ProxyConfig{RemoteURL: remote}
	if mod != nil {
		mod(cfg)
	}
	return &core.Repository{Name: name, Format: "raw", Type: core.RepoProxy, BlobStore: "default", Online: true,
		Config: core.RepoConfig{Proxy: cfg}}
}

func upstreamFor(t *testing.T, m *Manager, repo *core.Repository) *upstream {
	t.Helper()
	u, err := m.forRepository(repo)
	if err != nil {
		t.Fatalf("ForRepository: %v", err)
	}
	return u
}

func body(t *testing.T, r *core.UpstreamResponse) string {
	t.Helper()
	defer func() { _ = r.Body.Close() }()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func fetchOK(t *testing.T, u core.Upstream, req core.UpstreamRequest) (*core.UpstreamResponse, string) {
	t.Helper()
	resp, err := u.Fetch(context.Background(), req)
	if err != nil {
		t.Fatalf("Fetch %s: %v", req.Path, err)
	}
	return resp, body(t, resp)
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// staticResolver resolves names from a map.
type staticResolver map[string][]netip.Addr

func (r staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := r[host]; ok {
		return a, nil
	}
	return nil, &netError{host}
}

type netError struct{ host string }

func (e *netError) Error() string { return "no such host " + e.host }

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
