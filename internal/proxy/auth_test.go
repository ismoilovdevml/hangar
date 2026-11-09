package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// fakeRegistry is an OCI registry that demands tokens from a token service.
type fakeRegistry struct {
	mu          sync.Mutex
	registryHit int
	tokenHit    int
	tokenAuth   []string // Authorization headers seen by the token service
	scopesSeen  []string
	realm       string // token endpoint; empty = same server /token
	wantUser    string
	wantPass    string
}

func (f *fakeRegistry) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registryHit, f.tokenHit
}

func (f *fakeRegistry) tokenHandler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.tokenHit++
	f.tokenAuth = append(f.tokenAuth, r.Header.Get("Authorization"))
	f.scopesSeen = append(f.scopesSeen, r.URL.Query().Get("scope"))
	f.mu.Unlock()
	if f.wantUser != "" {
		if u, p, ok := r.BasicAuth(); !ok || u != f.wantUser || p != f.wantPass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	if r.URL.Query().Get("service") != "test-registry" {
		http.Error(w, "bad service", http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok:" + r.URL.Query().Get("scope"), "expires_in": 300})
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		f.tokenHandler(w, r)
		return
	}
	f.mu.Lock()
	f.registryHit++
	f.mu.Unlock()
	name, _, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v2/"), "/manifests/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	scope := "repository:" + name + ":pull"
	if r.Header.Get("Authorization") != "Bearer tok:"+scope {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="test-registry",scope=%q`, f.realm, scope))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Docker-Content-Digest", "sha256:abc")
	_, _ = io.WriteString(w, `{"schemaVersion":2}`)
}

func TestTokenChallengeSameOriginWithCredentials(t *testing.T) {
	reg := &fakeRegistry{wantUser: "svc", wantPass: "s3cret"}
	srv := httptest.NewServer(reg)
	defer srv.Close()
	reg.realm = srv.URL + "/token"
	clock := newClock()
	m := newManager(t, func(o *Options) { o.Now = clock.Now })
	u := upstreamFor(t, m, proxyRepo("docker-proxy", srv.URL, func(c *core.ProxyConfig) {
		c.Auth = &core.UpstreamAuth{Kind: core.UpstreamAuthBasic, Username: "svc", Password: "s3cret"}
	}))
	get := func(name string) {
		t.Helper()
		resp, b := fetchOK(t, u, core.UpstreamRequest{Path: "v2/" + name + "/manifests/latest"})
		if resp.StatusCode != http.StatusOK || b != `{"schemaVersion":2}` {
			t.Fatalf("%s: status %d body %q", name, resp.StatusCode, b)
		}
	}

	get("library/nginx")
	if r, tk := reg.counts(); r != 2 || tk != 1 {
		t.Fatalf("first fetch: registry=%d token=%d, want 2/1", r, tk)
	}
	get("library/nginx") // token cached for the scope: sent up front
	if r, tk := reg.counts(); r != 3 || tk != 1 {
		t.Fatalf("second fetch: registry=%d token=%d, want 3/1", r, tk)
	}
	get("library/redis") // other scope: new token
	if r, tk := reg.counts(); r != 5 || tk != 2 {
		t.Fatalf("other scope: registry=%d token=%d, want 5/2", r, tk)
	}
	clock.Advance(10 * time.Minute) // past expires_in
	get("library/nginx")
	if r, tk := reg.counts(); r != 7 || tk != 3 {
		t.Fatalf("after expiry: registry=%d token=%d, want 7/3", r, tk)
	}
	if !strings.HasPrefix(reg.tokenAuth[0], "Basic ") {
		t.Fatalf("token service must receive the upstream credentials, got %q", reg.tokenAuth[0])
	}
	if want := []string{"repository:library/nginx:pull", "repository:library/redis:pull", "repository:library/nginx:pull"}; !reflect.DeepEqual(reg.scopesSeen, want) {
		t.Fatalf("scopes %v", reg.scopesSeen)
	}
}

func TestTokenChallengeAnonymousAndDenied(t *testing.T) {
	tokens := &fakeRegistry{}
	tokenSrv := httptest.NewServer(http.HandlerFunc(tokens.tokenHandler))
	defer tokenSrv.Close()
	reg := &fakeRegistry{realm: tokenSrv.URL + "/token"}
	srv := httptest.NewServer(reg)
	defer srv.Close()
	m := newManager(t, nil)

	// The realm is plain HTTP on another origin: credentials must not be sent
	// there, the token is requested anonymously.
	u := upstreamFor(t, m, proxyRepo("anon", srv.URL, func(c *core.ProxyConfig) {
		c.Auth = &core.UpstreamAuth{Kind: core.UpstreamAuthBasic, Username: "svc", Password: "s3cret"}
	}))
	resp, _ := fetchOK(t, u, core.UpstreamRequest{Path: "v2/library/alpine/manifests/3"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if tokens.tokenAuth[0] != "" {
		t.Fatalf("credentials leaked to a plain-HTTP foreign realm: %q", tokens.tokenAuth[0])
	}

	// A token service that refuses: the 401 is returned and the remote is blocked.
	tokens.wantUser, tokens.wantPass = "someone", "else"
	repo := proxyRepo("denied", srv.URL, nil)
	u2 := upstreamFor(t, m, repo)
	resp, _ = fetchOK(t, u2, core.UpstreamRequest{Path: "v2/library/alpine/manifests/3"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("denied: status %d", resp.StatusCode)
	}
	if h := m.Health(repo); h.State != StateBlocked || h.Reason != ReasonAuthFailed {
		t.Fatalf("denied: %+v, want blocked by auth_failed", h)
	}
}

func TestCallerAuthorizationIsLeftAlone(t *testing.T) {
	reg := &fakeRegistry{}
	srv := httptest.NewServer(reg)
	defer srv.Close()
	reg.realm = srv.URL + "/token"
	m := newManager(t, nil)
	u := upstreamFor(t, m, proxyRepo("p", srv.URL, nil))
	resp, _ := fetchOK(t, u, core.UpstreamRequest{Path: "v2/a/manifests/1", Header: http.Header{"Authorization": {"Bearer mine"}}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if _, tk := reg.counts(); tk != 0 {
		t.Fatalf("token dance must not run when the caller set Authorization")
	}
}

func TestParseChallenges(t *testing.T) {
	tests := []struct {
		in   []string
		want []challenge
	}{
		{[]string{`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`},
			[]challenge{{"bearer", map[string]string{"realm": "https://auth.docker.io/token", "service": "registry.docker.io", "scope": "repository:library/nginx:pull"}}}},
		{[]string{`Basic realm="Registry Realm", Bearer realm="https://ghcr.io/token", service="ghcr.io"`},
			[]challenge{{"basic", map[string]string{"realm": "Registry Realm"}}, {"bearer", map[string]string{"realm": "https://ghcr.io/token", "service": "ghcr.io"}}}},
		{[]string{`Basic realm="a \"quoted\" realm"`, `Bearer realm=https://x/token,scope="a:b:pull a:c:pull"`},
			[]challenge{{"basic", map[string]string{"realm": `a "quoted" realm`}}, {"bearer", map[string]string{"realm": "https://x/token", "scope": "a:b:pull a:c:pull"}}}},
		{[]string{""}, nil},
	}
	for _, tc := range tests {
		if got := parseChallenges(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseChallenges(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestScopeKey(t *testing.T) {
	tests := map[string]string{
		"v2/library/nginx/manifests/1.27":      "v2/library/nginx",
		"/v2/acme/team/app/blobs/sha256:abc":   "v2/acme/team/app",
		"v2/library/nginx/tags/list?n=10":      "v2/library/nginx",
		"v2/a/manifests/manifests/x":           "v2/a/manifests",
		"v2/":                                  "v2/",
		"lodash":                               "",
		"org/acme/lib/1.0/lib-1.0.jar":         "",
		"v2/x/referrers/sha256:0123456789abcd": "v2/x",
	}
	for in, want := range tests {
		if got := scopeKey(in); got != want {
			t.Errorf("scopeKey(%q) = %q, want %q", in, got, want)
		}
	}
}
