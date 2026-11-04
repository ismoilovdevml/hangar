package httpclient

import (
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestProxySelection(t *testing.T) {
	tests := []struct {
		name   string
		opts   Options
		cfg    core.UpstreamHTTP
		target string
		want   string // "" = direct
	}{
		{"no proxy anywhere", Options{environ: env(nil)}, core.UpstreamHTTP{}, "https://repo1.maven.org/maven2", ""},
		{"global proxy", Options{Proxy: "http://proxy.corp:3128", environ: env(nil)}, core.UpstreamHTTP{}, "https://registry.npmjs.org/", "http://proxy.corp:3128"},
		{"global with credentials", Options{Proxy: "http://proxy.corp:3128", ProxyUsername: "svc", ProxyPassword: "p@ss", environ: env(nil)},
			core.UpstreamHTTP{}, "https://registry.npmjs.org/", "http://svc:p%40ss@proxy.corp:3128"},
		{"global no_proxy suffix", Options{Proxy: "http://proxy.corp:3128", NoProxy: []string{".corp.example", "10.0.0.0/8"}, environ: env(nil)},
			core.UpstreamHTTP{}, "https://artifacts.corp.example/repo", ""},
		{"global no_proxy cidr", Options{Proxy: "http://proxy.corp:3128", NoProxy: []string{"10.0.0.0/8"}, environ: env(nil)},
			core.UpstreamHTTP{}, "http://10.1.2.3:8081/", ""},
		{"environment https", Options{UseEnvironment: true, environ: env(map[string]string{"HTTPS_PROXY": "http://envproxy:8080"})},
			core.UpstreamHTTP{Proxy: core.ProxyInherit}, "https://proxy.golang.org/", "http://envproxy:8080"},
		{"environment lower-case + NO_PROXY", Options{UseEnvironment: true, environ: env(map[string]string{"https_proxy": "http://envproxy:8080", "no_proxy": "internal.example"})},
			core.UpstreamHTTP{}, "https://internal.example/", ""},
		{"environment ignored when disabled", Options{UseEnvironment: false, environ: env(map[string]string{"HTTPS_PROXY": "http://envproxy:8080"})},
			core.UpstreamHTTP{}, "https://proxy.golang.org/", ""},
		{"per-remote none beats global", Options{Proxy: "http://proxy.corp:3128", environ: env(nil)},
			core.UpstreamHTTP{Proxy: core.ProxyNone}, "https://registry.npmjs.org/", ""},
		{"per-remote socks5", Options{Proxy: "http://proxy.corp:3128", environ: env(nil)},
			core.UpstreamHTTP{Proxy: "socks5://dmz-socks:1080"}, "https://registry-1.docker.io/v2/", "socks5://dmz-socks:1080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := New(tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.ProxyFor(tt.target, tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			gotS := ""
			if got != nil {
				gotS = got.String()
			}
			if gotS != tt.want {
				t.Errorf("ProxyFor = %q, want %q", gotS, tt.want)
			}
		})
	}
}

func TestInvalidProxy(t *testing.T) {
	if _, err := New(Options{Proxy: "ftp://proxy:21"}); err == nil {
		t.Error("ftp proxy accepted")
	}
	f, _ := New(Options{environ: env(nil)})
	if _, err := f.ForUpstream("https://x.example", core.UpstreamHTTP{Proxy: "not a url"}, nil); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("bad per-remote proxy = %v", err)
	}
	if _, err := f.ForUpstream("ftp://x.example", core.UpstreamHTTP{}, nil); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("ftp remote = %v", err)
	}
	if _, err := f.ForUpstream("https://x.example", core.UpstreamHTTP{}, &core.UpstreamAuth{Kind: "header", Token: "t"}); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("header auth without name = %v", err)
	}
}

// A forward proxy receives absolute-form requests; check credentials and auth placement end to end.
func TestRequestThroughProxyWithAuth(t *testing.T) {
	type seen struct{ url, auth, proxyAuth, ua string }
	var mu sync.Mutex
	var got []seen
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, seen{r.URL.String(), r.Header.Get("Authorization"), r.Header.Get("Proxy-Authorization"), r.Header.Get("User-Agent")})
		mu.Unlock()
		if r.URL.Host == "repo.example" {
			http.Redirect(w, r, "http://cdn.example/blob", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer proxy.Close()

	f, err := New(Options{Proxy: proxy.URL, ProxyUsername: "svc", ProxyPassword: "pw", UserAgent: "Hangar/test", environ: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.ForUpstream("http://repo.example/maven2", core.UpstreamHTTP{}, &core.UpstreamAuth{Kind: "basic", Username: "u", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("http://repo.example/maven2/a.jar")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" || len(got) != 2 {
		t.Fatalf("body=%q requests=%+v", body, got)
	}
	if got[0].auth == "" || !strings.HasPrefix(got[0].auth, "Basic ") {
		t.Errorf("remote request missing basic auth: %+v", got[0])
	}
	if got[1].url != "http://cdn.example/blob" || got[1].auth != "" {
		t.Errorf("credentials leaked to redirect target: %+v", got[1])
	}
	for _, s := range got {
		if !strings.HasPrefix(s.proxyAuth, "Basic ") || s.ua != "Hangar/test" {
			t.Errorf("proxy auth / UA missing: %+v", s)
		}
	}
	c2, _ := f.ForUpstream("http://repo.example/maven2", core.UpstreamHTTP{}, &core.UpstreamAuth{Kind: "basic", Username: "u", Password: "p"})
	if c2 != c {
		t.Error("identical config should reuse the cached client")
	}
}

func TestHeaderAuthAndCallerAuthorizationWins(t *testing.T) {
	var gotHeader, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotHeader, gotAuth = r.Header.Get("Private-Token"), r.Header.Get("Authorization")
	}))
	defer srv.Close()
	f, _ := New(Options{environ: env(nil)})
	c, err := f.ForUpstream(srv.URL, core.UpstreamHTTP{Proxy: core.ProxyNone}, &core.UpstreamAuth{Kind: "header", HeaderName: "Private-Token", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/x", nil)
	req.Header.Set("Authorization", "Bearer from-token-service")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotAuth != "Bearer from-token-service" {
		t.Errorf("caller Authorization overridden: %q", gotAuth)
	}
	// Caller set Authorization, so configured credentials are not added at all.
	if gotHeader != "" {
		t.Errorf("header auth added despite caller Authorization: %q", gotHeader)
	}
	resp, err = c.Get(srv.URL + "/y")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if gotHeader != "tok" {
		t.Errorf("header auth = %q", gotHeader)
	}
}

func TestCABundleAndInsecure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "tls") }))
	defer srv.Close()
	f, _ := New(Options{environ: env(nil), ConnectTimeout: 2 * time.Second})

	plain, _ := f.ForUpstream(srv.URL, core.UpstreamHTTP{}, nil)
	if _, err := plain.Get(srv.URL); err == nil {
		t.Fatal("self-signed server trusted without CA bundle")
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	withCA, err := f.ForUpstream(srv.URL, core.UpstreamHTTP{CABundle: caPEM}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := withCA.Get(srv.URL)
	if err != nil {
		t.Fatalf("with CA bundle: %v", err)
	}
	_ = resp.Body.Close()
	insecure, _ := f.ForUpstream(srv.URL, core.UpstreamHTTP{InsecureSkipVerify: true}, nil)
	resp, err = insecure.Get(srv.URL)
	if err != nil {
		t.Fatalf("insecure: %v", err)
	}
	_ = resp.Body.Close()
	if _, err := f.ForUpstream(srv.URL, core.UpstreamHTTP{CABundle: "garbage"}, nil); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("garbage CA = %v", err)
	}
}

func TestReadTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	f, _ := New(Options{environ: env(nil)})
	c, _ := f.ForUpstream(srv.URL, core.UpstreamHTTP{ReadTimeout: core.Duration(100 * time.Millisecond)}, nil)
	start := time.Now()
	if _, err := c.Get(srv.URL); err == nil {
		t.Fatal("expected header timeout")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not applied")
	}
}

// errLoopback blocks loopback addresses, like the SSRF guard does.
var errLoopback = errors.New("loopback blocked")

func blockLoopback(a netip.Addr) error {
	if a.IsLoopback() {
		return errLoopback
	}
	return nil
}

// DialGuard sees the address actually dialled: a host name that resolves to a
// blocked address (DNS rebinding) is refused at connect time.
func TestDialGuard(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "remote") }))
	defer remote.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(remote.URL, "http://"), ":")
	byName := "http://localhost:" + port + "/x" // resolves to 127.0.0.1 only at dial time

	get := func(f *Factory, target string) (string, error) {
		t.Helper()
		c, err := f.ForUpstream(target, core.UpstreamHTTP{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Get(target)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}

	f, _ := New(Options{environ: env(nil), DialGuard: blockLoopback})
	if _, err := get(f, byName); !errors.Is(err, errLoopback) {
		t.Fatalf("direct dial to loopback by name = %v, want errLoopback", err)
	}
	if _, err := get(f, remote.URL); !errors.Is(err, errLoopback) {
		t.Fatalf("direct dial to loopback IP = %v, want errLoopback", err)
	}

	// Allow-listed host names skip the dial check.
	f, _ = New(Options{environ: env(nil), DialGuard: blockLoopback, DialAllowHost: func(h string) bool { return h == "localhost" }})
	if body, err := get(f, byName); err != nil || body != "remote" {
		t.Fatalf("allow-listed host = %q, %v", body, err)
	}

	// Connections to the configured outbound proxy are not checked: the
	// operator chose it and the proxy applies its own policy.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "via proxy") }))
	defer proxy.Close()
	f, _ = New(Options{Proxy: proxy.URL, environ: env(nil), DialGuard: blockLoopback})
	if body, err := get(f, "http://public.example/x"); err != nil || body != "via proxy" {
		t.Fatalf("through proxy = %q, %v", body, err)
	}
	// ... but no_proxy targets are dialled directly and checked.
	f, _ = New(Options{Proxy: proxy.URL, NoProxy: []string{"localhost"}, environ: env(nil), DialGuard: blockLoopback})
	if _, err := get(f, byName); !errors.Is(err, errLoopback) {
		t.Fatalf("no_proxy target = %v, want errLoopback", err)
	}
	// Per-remote proxies count as proxies too.
	f, _ = New(Options{environ: env(nil), DialGuard: blockLoopback})
	c, err := f.ForUpstream("http://public.example/", core.UpstreamHTTP{Proxy: proxy.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("http://public.example/x")
	if err != nil {
		t.Fatalf("per-remote proxy = %v", err)
	}
	_ = resp.Body.Close()
	if u, _ := url.Parse(proxy.URL); !f.proxyAddrs[u.Host] {
		t.Errorf("per-remote proxy %s not registered: %v", u.Host, f.proxyAddrs)
	}
}
