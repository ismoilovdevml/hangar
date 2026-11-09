package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
)

func TestGuardCheckAddr(t *testing.T) {
	g, err := NewGuard([]string{"10.20.0.0/16", "192.168.1.5"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		addr    string
		blocked bool
	}{
		{"127.0.0.1", true},
		{"127.8.9.10", true},
		{"169.254.169.254", true},
		{"10.0.0.1", true},
		{"172.16.5.4", true},
		{"192.168.0.1", true},
		{"100.64.1.1", true},
		{"0.0.0.0", true},
		{"224.0.0.1", true},
		{"255.255.255.255", true},
		{"::1", true},
		{"::", true},
		{"fe80::1", true},
		{"fd00::1", true},
		{"fc00::1", true},
		{"ff02::1", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"64:ff9b::a00:1", true},    // NAT64 of 10.0.0.1
		{"2002:a9fe:a9fe::1", true}, // 6to4 of 169.254.169.254
		{"8.8.8.8", false},
		{"151.101.0.223", false},
		{"2606:4700::6810:84e5", false},
		{"64:ff9b::808:808", false}, // NAT64 of 8.8.8.8
		{"10.20.3.4", false},        // allow-listed CIDR
		{"192.168.1.5", false},      // allow-listed IP
		{"192.168.1.6", true},
	}
	for _, tc := range tests {
		err := g.CheckAddr(netip.MustParseAddr(tc.addr))
		if got := errors.Is(err, ErrBlocked); got != tc.blocked {
			t.Errorf("%s: blocked=%v, want %v (%v)", tc.addr, got, tc.blocked, err)
		}
	}
}

func TestGuardCheckURL(t *testing.T) {
	g, err := NewGuard([]string{"mirror.corp.example", ".internal.example"})
	if err != nil {
		t.Fatal(err)
	}
	g.resolver = staticResolver{
		"registry.npmjs.org":  {netip.MustParseAddr("104.16.0.35")},
		"metadata.evil.test":  {netip.MustParseAddr("169.254.169.254")},
		"mixed.evil.test":     {netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")},
		"mirror.corp.example": {netip.MustParseAddr("10.1.1.1")},
		"a.internal.example":  {netip.MustParseAddr("10.1.1.2")},
	}
	tests := []struct {
		url     string
		proxied bool
		blocked bool
		err     bool
	}{
		{"https://registry.npmjs.org/lodash", false, false, false},
		{"http://169.254.169.254/latest/meta-data/", false, true, true},
		{"http://169.254.169.254/latest/meta-data/", true, true, true}, // literal IPs are checked even via a proxy
		{"http://metadata.evil.test/", false, true, true},
		{"http://mixed.evil.test/", false, true, true},
		{"https://mirror.corp.example/repository/x", false, false, false}, // allow-listed host
		{"https://a.internal.example/", false, false, false},              // allow-listed suffix
		{"http://[::1]:8080/", false, true, true},
		{"http://[fe80::1%25en0]/", false, true, true},
		{"file:///etc/passwd", false, true, true},
		{"gopher://x/", false, true, true},
		{"http:///nohost", false, true, true},
		{"https://unknown.example.test/", true, false, false}, // proxy resolves
		{"https://unknown.example.test/", false, false, true}, // resolution error, not a policy block
	}
	for _, tc := range tests {
		u, err := url.Parse(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		err = g.Check(context.Background(), u, tc.proxied)
		if (err != nil) != tc.err || errors.Is(err, ErrBlocked) != tc.blocked {
			t.Errorf("%s proxied=%v: err=%v, want err=%v blocked=%v", tc.url, tc.proxied, err, tc.err, tc.blocked)
		}
	}

	if err := g.ValidateRemote(context.Background(), "http://metadata.evil.test/", false); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("ValidateRemote blocked: want ErrInvalid, got %v", err)
	}
	if err := g.ValidateRemote(context.Background(), "https://unknown.example.test/", false); err != nil {
		t.Errorf("ValidateRemote unresolvable: want nil, got %v", err)
	}
	if _, err := NewGuard([]string{"http://bad/entry"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad allow-list entry: want ErrInvalid, got %v", err)
	}
}

func TestFetchBlocksInternalRemotes(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	// Default guard: nothing internal is allowed, including the test server.
	m, err := New(Options{HTTPClients: newFactory(t, httpclient.Options{})})
	if err != nil {
		t.Fatal(err)
	}
	m.opts.Guard.resolver = staticResolver{"metadata.evil.test": {netip.MustParseAddr("169.254.169.254")}}
	for _, remote := range []string{
		srv.URL, // 127.0.0.1
		strings.Replace(srv.URL, "127.0.0.1", "[::1]", 1),
		"http://169.254.169.254/latest/meta-data",
		"http://10.1.2.3:8081/repository/maven-central",
		"http://metadata.evil.test/",
	} {
		repo := proxyRepo("p", remote, nil)
		u := upstreamFor(t, m, repo)
		_, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "x"})
		if !errors.Is(err, ErrBlocked) || !errors.Is(err, core.ErrUpstream) {
			t.Errorf("%s: want ErrBlocked+ErrUpstream, got %v", remote, err)
		}
		if h := m.Health(repo); h.State != StateBlocked || h.Reason != ReasonAddressBlocked {
			t.Errorf("%s: %+v, want blocked by address_blocked", remote, h)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("blocked remote was contacted %d times", hits.Load())
	}
}

func TestFetchBlocksRedirectToInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-private":
			http.Redirect(w, r, "http://10.0.0.1/secret", http.StatusFound)
		case "/to-metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/iam", http.StatusTemporaryRedirect)
		case "/to-gopher":
			http.Redirect(w, r, "gopher://example.com/", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/ok-hop":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			_, _ = io.WriteString(w, "landed")
		}
	}))
	defer srv.Close()
	m := newManager(t, func(o *Options) { o.Retries = -1 })
	u := upstreamFor(t, m, proxyRepo("p", srv.URL, nil))
	for _, p := range []string{"to-private", "to-metadata", "to-gopher"} {
		if _, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: p}); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: want ErrBlocked, got %v", p, err)
		}
	}
	if _, err := u.Fetch(context.Background(), core.UpstreamRequest{Path: "loop"}); err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Errorf("loop: want redirect limit error, got %v", err)
	}
	if _, b := fetchOK(t, u, core.UpstreamRequest{Path: "ok-hop"}); b != "landed" {
		t.Errorf("allowed redirect: body %q", b)
	}
}

func TestOutboundProxyIsUsed(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.String()+" auth="+r.Header.Get("Private-Token"))
		mu.Unlock()
		_, _ = io.WriteString(w, "via proxy")
	}))
	defer fwd.Close()
	g, err := NewGuard(nil)
	if err != nil {
		t.Fatal(err)
	}
	// The remote's name only resolves on the proxy's side, as in many corporate
	// networks: the guard lets it through because the request is proxied.
	g.resolver = staticResolver{}
	m, err := New(Options{HTTPClients: newFactory(t, httpclient.Options{Proxy: fwd.URL}), Guard: g})
	if err != nil {
		t.Fatal(err)
	}
	u := upstreamFor(t, m, proxyRepo("p", "http://registry.hangar.test/api", func(c *core.ProxyConfig) {
		c.Auth = &core.UpstreamAuth{Kind: core.UpstreamAuthHeader, HeaderName: "Private-Token", Token: "t0k"}
	}))
	if _, b := fetchOK(t, u, core.UpstreamRequest{Path: "pkg/x"}); b != "via proxy" {
		t.Fatalf("body %q", b)
	}
	mu.Lock()
	got := strings.Join(seen, "|")
	mu.Unlock()
	if got != "GET http://registry.hangar.test/api/pkg/x auth=t0k" {
		t.Fatalf("proxy saw %q", got)
	}

	// Literal internal addresses stay blocked even through the proxy.
	u2 := upstreamFor(t, m, proxyRepo("p2", "http://10.9.8.7/", nil))
	if _, err := u2.Fetch(context.Background(), core.UpstreamRequest{Path: "x"}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("literal private IP via proxy: want ErrBlocked, got %v", err)
	}

	// Per-repository "none" bypasses the proxy: the name no longer resolves.
	u3 := upstreamFor(t, m, proxyRepo("p3", "http://registry.hangar.test/api", func(c *core.ProxyConfig) { c.HTTP.Proxy = core.ProxyNone }))
	_, err = u3.Fetch(context.Background(), core.UpstreamRequest{Path: "x"})
	if err == nil || errors.Is(err, ErrBlocked) || !errors.Is(err, core.ErrUpstream) {
		t.Fatalf("direct, unresolvable: want plain ErrUpstream, got %v", err)
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("proxy contacted %d times, want 1", n)
	}
}
