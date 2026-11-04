package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// hostOf returns the host:port of a test server.
func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func TestRelatedHostFetch(t *testing.T) {
	var remoteAuth, cdnAuth atomic.Value
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteAuth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "remote:"+r.URL.Path)
	}))
	defer remote.Close()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "cdn:"+r.URL.Path)
	}))
	defer cdn.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an unrelated host was contacted")
	}))
	defer other.Close()

	m := newManager(t, nil)
	repo := proxyRepo("pypi-proxy", remote.URL+"/simple/", func(c *core.ProxyConfig) {
		c.Auth = &core.UpstreamAuth{Kind: core.UpstreamAuthBasic, Username: "u", Password: "secret"}
		c.RelatedHosts = []string{hostOf(t, cdn)}
	})
	u := upstreamFor(t, m, repo)

	// A URL on the remote's own origin is fetched with its credentials,
	// wherever the path is.
	if _, b := fetchOK(t, u, core.UpstreamRequest{URL: remote.URL + "/files/a.whl"}); b != "remote:/files/a.whl" {
		t.Fatalf("own-origin URL: %q", b)
	}
	if a, _ := remoteAuth.Load().(string); !strings.HasPrefix(a, "Basic ") {
		t.Errorf("remote did not get its credentials: %q", a)
	}

	// A related host is fetched without them.
	if _, b := fetchOK(t, u, core.UpstreamRequest{URL: cdn.URL + "/pkgs/a.tar.gz#frag"}); b != "cdn:/pkgs/a.tar.gz" {
		t.Fatalf("related URL: %q", b)
	}
	if a, _ := cdnAuth.Load().(string); a != "" {
		t.Errorf("related host got credentials: %q", a)
	}

	// Request-level related hosts (format defaults, service index hosts).
	repo2 := proxyRepo("nuget-proxy", remote.URL, nil)
	u2 := upstreamFor(t, m, repo2)
	if _, err := u2.Fetch(context.Background(), core.UpstreamRequest{URL: cdn.URL + "/q"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("host not related: want ErrInvalid, got %v", err)
	}
	if _, b := fetchOK(t, u2, core.UpstreamRequest{URL: cdn.URL + "/q", RelatedHosts: []string{hostOf(t, cdn)}}); b != "cdn:/q" {
		t.Fatalf("request-level related host: %q", b)
	}

	// 404s from related hosts are negative-cached like the remote's.
	for range 2 {
		resp, _ := fetchOK(t, u, core.UpstreamRequest{URL: cdn.URL + "/missing"})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("missing: %d", resp.StatusCode)
		}
	}
	if n := u.neg.purge(u.now()); n != 1 {
		t.Errorf("negative entries = %d, want 1", n)
	}

	for _, bad := range []string{
		other.URL + "/x",                                            // unrelated host
		"ftp://" + hostOf(t, cdn) + "/x",                            // scheme
		"http://user:pw@" + hostOf(t, cdn) + "/x",                   // credentials in the URL
		"/relative/path",                                            // not absolute
		"https://" + strings.Split(hostOf(t, cdn), ":")[0] + ":1/x", // related host, other port
	} {
		if _, err := u.Fetch(context.Background(), core.UpstreamRequest{URL: bad}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("URL %q: want ErrInvalid, got %v", bad, err)
		}
	}
}

func TestRelatedHostSSRFGuard(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("blocked related host was contacted")
	}))
	defer cdn.Close()
	// The default guard blocks loopback, related hosts included.
	g, err := NewGuard(nil)
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(t, func(o *Options) { o.Guard = g })
	u := upstreamFor(t, m, proxyRepo("p", "https://registry.example", nil))
	_, err = u.Fetch(context.Background(), core.UpstreamRequest{URL: cdn.URL + "/x", RelatedHosts: []string{hostOf(t, cdn)}})
	if !errors.Is(err, ErrBlocked) || !errors.Is(err, core.ErrUpstream) {
		t.Fatalf("want ErrBlocked, got %v", err)
	}
}

func TestRelatedHostHasItsOwnBreaker(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer remote.Close()
	var cdnHits atomic.Int32
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cdnHits.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer cdn.Close()
	m := newManager(t, func(o *Options) { o.Retries = -1 })
	repo := proxyRepo("p", remote.URL, func(c *core.ProxyConfig) {
		c.CircuitFailures = 2
		c.RelatedHosts = []string{hostOf(t, cdn)}
	})
	u := upstreamFor(t, m, repo)
	for range 2 {
		fetchOK(t, u, core.UpstreamRequest{URL: cdn.URL + "/f"})
	}
	if _, err := u.Fetch(context.Background(), core.UpstreamRequest{URL: cdn.URL + "/f"}); !errors.Is(err, core.ErrUpstream) {
		t.Fatalf("related breaker open: want ErrUpstream, got %v", err)
	}
	if cdnHits.Load() != 2 {
		t.Errorf("cdn hits = %d, want 2", cdnHits.Load())
	}
	if _, b := fetchOK(t, u, core.UpstreamRequest{Path: "x"}); b != "ok" {
		t.Fatalf("remote must stay usable: %q", b)
	}
	if st := m.Health(repo).State; st != StateHealthy {
		t.Errorf("remote state = %s, want healthy", st)
	}
}

func TestMatchHost(t *testing.T) {
	for _, tt := range []struct {
		patterns []string
		host     string
		want     bool
	}{
		{[]string{"files.pythonhosted.org"}, "files.pythonhosted.org", true},
		{[]string{"Files.PythonHosted.org"}, "files.pythonhosted.org:443", true},
		{[]string{"cdn.example:8443"}, "cdn.example:8443", true},
		{[]string{"cdn.example:8443"}, "cdn.example", false},
		{[]string{".nuget.org"}, "azuresearch-usnc.nuget.org", true},
		{[]string{".nuget.org"}, "nuget.org", false},
		{[]string{".nuget.org"}, "evilnuget.org", false},
		{[]string{"example.org"}, "sub.example.org", false},
		{nil, "x", false},
	} {
		if got := core.MatchHost(tt.patterns, tt.host); got != tt.want {
			t.Errorf("MatchHost(%v, %q) = %v, want %v", tt.patterns, tt.host, got, tt.want)
		}
	}
	for p, ok := range map[string]bool{"cdn.example": true, ".example.org": true, "cdn.example:443": true,
		"": false, "a/b": false, "*.example": false, "u@h": false, "..x": false} {
		if err := core.ValidateHostPattern(p); (err == nil) != ok {
			t.Errorf("ValidateHostPattern(%q) = %v", p, err)
		}
	}
}

func TestMaxConcurrentRequests(t *testing.T) {
	var cur, peak atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cur.Add(1)
		defer cur.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	m := newManager(t, nil)
	u := upstreamFor(t, m, proxyRepo("p", srv.URL, func(c *core.ProxyConfig) { c.MaxConcurrentRequests = 2 }))

	var wg sync.WaitGroup
	for i := range 5 {
		wg.Go(func() {
			// Different paths: no request collapsing.
			resp, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "f" + string(rune('a'+i))})
			if err != nil {
				t.Error(err)
				return
			}
			if b := body(t, resp); b != "ok" {
				t.Errorf("body %q", b)
			}
		})
	}
	waitFor(t, "two requests in flight", func() bool { return cur.Load() == 2 })
	close(release)
	wg.Wait()
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrency = %d, want 2", p)
	}

	// A caller giving up while waiting for a slot gets its context error.
	u.slots <- struct{}{}
	u.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := u.Fetch(ctx, core.UpstreamRequest{Path: "late"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
