package httpclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// The client cache keeps the most recently requested clients only: a
// client per README image host, or per old version of a repository's
// settings, must not accumulate for the life of the process. A client
// dropped from the cache keeps working for its holder and still follows
// SetOutbound.
func TestClientCacheIsBounded(t *testing.T) {
	var (
		mu   sync.Mutex
		hits []string
	)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "direct") }))
	t.Cleanup(remote.Close)
	corp := forwardProxy(t, "corp", &hits, &mu)

	f, err := New(Options{environ: env(nil), MaxClients: 2})
	if err != nil {
		t.Fatal(err)
	}
	client := func(host string) *http.Client {
		t.Helper()
		c, err := f.ForUpstream("http://"+host+"/", core.UpstreamHTTP{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := client("a.example"), client("b.example")
	if client("a.example") != a {
		t.Fatal("a repeated request for the same remote built a new client")
	}
	// a was requested last, so c pushes b out.
	_ = client("c.example")
	if client("a.example") != a {
		t.Error("the most recently requested client was evicted")
	}
	if client("b.example") == b {
		t.Error("the least recently requested client is still cached beyond MaxClients")
	}
	f.mu.Lock()
	n := len(f.clients)
	f.mu.Unlock()
	if n != 2 {
		t.Errorf("cache holds %d clients, want 2", n)
	}

	// The evicted client still sends, and a settings change reaches it.
	get := func(c *http.Client, target string) string {
		t.Helper()
		resp, err := c.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	if got := get(b, remote.URL+"/x"); got != "direct" {
		t.Fatalf("evicted client = %q", got)
	}
	if err := f.SetOutbound(Outbound{Default: "corp", Proxies: []Proxy{{Name: "corp", URL: corp.URL}}}); err != nil {
		t.Fatal(err)
	}
	// (Loopback targets are never proxied, so ask for the remote by name.)
	if got := get(b, "http://b.example/x"); got != "corp" {
		t.Fatalf("evicted client after SetOutbound went through %q, want the new default proxy", got)
	}
}

// Requests do not serialize on the factory: the transport in effect is found
// without its lock, so a request goes out while the lock is held (by a
// SetOutbound or a ForUpstream of another caller). Before, every request
// took the lock and JSON-encoded the remote's settings to find its transport.
func TestRequestsDoNotTakeFactoryLock(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(remote.Close)
	f, err := New(Options{environ: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.ForUpstream(remote.URL, core.UpstreamHTTP{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, remote.URL+"/x", nil)
		resp, err := c.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("request while the factory lock is held: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("request blocked on the factory lock")
	}
}
