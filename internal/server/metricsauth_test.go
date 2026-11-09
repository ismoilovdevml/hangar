package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/observability"
)

const (
	testMetricsToken    = "scrape-token-0123456789"
	testMetricsUser     = "prometheus"
	testMetricsPassword = "correct-horse-battery"
)

func metricsRouter(t *testing.T, o MetricsAuthOptions, sec Security) http.Handler {
	t.Helper()
	auth, err := NewMetricsAuth(o)
	if err != nil {
		t.Fatal(err)
	}
	trusted, _ := ParseTrustedProxies([]string{"10.0.0.0/8"})
	return NewRouter(RouterOptions{
		Logger: discard(), Metrics: observability.NewMetrics(), MetricsPath: "/metrics", MetricsAuth: auth,
		TrustedProxies: trusted, Security: sec,
	})
}

type scrape struct {
	name   string
	remote string // RemoteAddr host
	xff    string
	setup  func(*http.Request)
	code   int
}

func runScrapes(t *testing.T, h http.Handler, cases []scrape) {
	t.Helper()
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if c.remote != "" {
			req.RemoteAddr = net.JoinHostPort(c.remote, "40000")
		}
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.setup != nil {
			c.setup(req)
		}
		rec := serve(h, req)
		if rec.Code != c.code {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.code)
			continue
		}
		if c.code == http.StatusOK && !strings.Contains(rec.Body.String(), "hangar_") {
			t.Errorf("%s: no metrics in the body", c.name)
		}
		if c.code != http.StatusOK && strings.Contains(rec.Body.String(), "hangar_") {
			t.Errorf("%s: metrics leaked in a %d response", c.name, c.code)
		}
	}
}

func bearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func basic(u, p string) func(*http.Request) {
	return func(r *http.Request) { r.SetBasicAuth(u, p) }
}

func TestMetricsAuthNone(t *testing.T) {
	if a, err := NewMetricsAuth(MetricsAuthOptions{Mode: "none"}); a != nil || err != nil {
		t.Fatalf("none = %v, %v; want no guard", a, err)
	}
	runScrapes(t, metricsRouter(t, MetricsAuthOptions{}, Security{}), []scrape{
		{name: "anonymous", code: 200},
		{name: "with credentials", setup: bearer("anything"), code: 200},
	})
}

func TestMetricsAuthBearer(t *testing.T) {
	h := metricsRouter(t, MetricsAuthOptions{Mode: "bearer", BearerToken: testMetricsToken}, Security{})
	runScrapes(t, h, []scrape{
		{name: "right token", setup: bearer(testMetricsToken), code: 200},
		{name: "scheme case", setup: func(r *http.Request) { r.Header.Set("Authorization", "bearer "+testMetricsToken) }, code: 200},
		{name: "no token", code: 401},
		{name: "wrong token", setup: bearer(testMetricsToken + "x"), code: 401},
		{name: "prefix of token", setup: bearer(testMetricsToken[:8]), code: 401},
		{name: "empty token", setup: bearer(""), code: 401},
		{name: "basic instead", setup: basic(testMetricsUser, testMetricsToken), code: 401},
	})
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="metrics"` {
		t.Errorf("challenge = %q", got)
	}
}

func TestMetricsAuthBasic(t *testing.T) {
	h := metricsRouter(t, MetricsAuthOptions{Mode: "basic", Username: testMetricsUser, Password: testMetricsPassword}, Security{})
	runScrapes(t, h, []scrape{
		{name: "right credentials", setup: basic(testMetricsUser, testMetricsPassword), code: 200},
		{name: "none", code: 401},
		{name: "wrong password", setup: basic(testMetricsUser, "correct-horse-batterz"), code: 401},
		{name: "wrong user", setup: basic("admin", testMetricsPassword), code: 401},
		{name: "bearer instead", setup: bearer(testMetricsPassword), code: 401},
	})
	rec := serve(h, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, `Basic realm="metrics"`) {
		t.Errorf("challenge = %q", got)
	}
}

// The allowlist uses the client IP as RequestInfo resolves it:
// X-Forwarded-For counts only when the peer is a trusted proxy (10/8 here).
func TestMetricsAllowedCIDRs(t *testing.T) {
	h := metricsRouter(t, MetricsAuthOptions{AllowedCIDRs: []string{"192.0.2.0/24", "2001:db8::1"}}, Security{})
	runScrapes(t, h, []scrape{
		{name: "allowed network", remote: "192.0.2.10", code: 200},
		{name: "allowed single IPv6", remote: "2001:db8::1", code: 200},
		{name: "other network", remote: "198.51.100.7", code: 403},
		{name: "via trusted proxy", remote: "10.1.2.3", xff: "192.0.2.10", code: 200},
		{name: "via trusted proxy, client outside", remote: "10.1.2.3", xff: "198.51.100.7", code: 403},
		{name: "spoofed header from untrusted peer", remote: "198.51.100.7", xff: "192.0.2.10", code: 403},
	})
}

// allowed_cidrs and credentials combine: both must pass.
func TestMetricsAllowedCIDRsWithBearer(t *testing.T) {
	h := metricsRouter(t, MetricsAuthOptions{Mode: "bearer", BearerToken: testMetricsToken, AllowedCIDRs: []string{"192.0.2.0/24"}}, Security{})
	runScrapes(t, h, []scrape{
		{name: "allowed and token", remote: "192.0.2.10", setup: bearer(testMetricsToken), code: 200},
		{name: "allowed, no token", remote: "192.0.2.10", code: 401},
		{name: "token, outside", remote: "198.51.100.7", setup: bearer(testMetricsToken), code: 403},
	})
}

// Guessing the token is throttled like any other failed authentication.
func TestMetricsAuthFailuresAreRateLimited(t *testing.T) {
	h := metricsRouter(t, MetricsAuthOptions{Mode: "bearer", BearerToken: testMetricsToken},
		Security{FailedAuthRate: Rate{PerMinute: 6, Burst: 2}})
	runScrapes(t, h, []scrape{
		{name: "guess 1", remote: "192.0.2.10", setup: bearer("guess-1"), code: 401},
		{name: "guess 2", remote: "192.0.2.10", setup: bearer("guess-2"), code: 401},
		{name: "guess 3", remote: "192.0.2.10", setup: bearer("guess-3"), code: 429},
		{name: "other client", remote: "192.0.2.11", setup: bearer(testMetricsToken), code: 200},
	})
}

func TestNewMetricsAuthRejectsBadOptions(t *testing.T) {
	for _, o := range []MetricsAuthOptions{
		{Mode: "bearer"},
		{Mode: "basic", Username: testMetricsUser},
		{Mode: "digest"},
		{AllowedCIDRs: []string{"not-a-network"}},
	} {
		if _, err := NewMetricsAuth(o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}
