package httpapi_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/auth/httpapi"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc/oidctest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

type ssoServer struct {
	*testServer
	issuer *oidctest.Issuer
}

// newSSOServer serves the API with OIDC sign-in against a fake issuer.
func newSSOServer(t *testing.T, cfg auth.Config, opts ...authtest.Option) *ssoServer {
	t.Helper()
	is := oidctest.New(t)
	cfg.OIDC.Enabled, cfg.OIDC.IssuerURL = true, is.URL
	cfg.OIDC.ClientID, cfg.OIDC.ClientSecret = oidctest.ClientID, oidctest.ClientSecret
	cfg.OIDC.AutoCreate = true
	cfg.OIDC.DisplayName = "Acme SSO"
	env := authtest.New(t, cfg, opts...)
	var h http.Handler
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	p, err := oidc.New(env.Svc.Config().OIDC, oidc.Options{RedirectURL: srv.URL + oidc.CallbackPath})
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Mount("/api/v1", httpapi.Routes(env.Svc, httpapi.Options{
		Organization: httpapi.Organization{Slug: "acme", Name: "Acme"}, BaseURL: srv.URL, OIDC: p,
	}))
	h = r
	return &ssoServer{testServer: &testServer{Server: srv, env: env}, issuer: is}
}

// sso walks a browser through /auth/oidc/start, the issuer and the callback,
// and returns where Hangar finally sent it (the app path).
func (c *client) sso(redirect string) *url.URL {
	c.t.Helper()
	hangar, _ := url.Parse(c.srv.URL)
	hc := &http.Client{Transport: c.hc.Transport, Jar: c.hc.Jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Host == hangar.Host && !strings.HasPrefix(req.URL.Path, "/api/") {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	resp, err := hc.Get(c.srv.URL + "/api/v1/auth/oidc/start?redirect=" + url.QueryEscape(redirect))
	if err != nil {
		c.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		c.t.Fatalf("sso: status %d at %s", resp.StatusCode, resp.Request.URL)
	}
	loc, err := resp.Location()
	if err != nil {
		c.t.Fatal(err)
	}
	return loc
}

func ssoUser(sub, name string, groups ...any) map[string]any {
	return map[string]any{"sub": sub, "preferred_username": name, "email": name + "@example.com", "name": name, "groups": groups}
}

func ssoConfig() auth.Config {
	return auth.Config{OIDC: auth.OIDCConfig{
		GroupRoles:    map[string][]string{"devops": {"Publisher"}, "hangar-admins": {"Admin"}},
		DefaultRoles:  []string{"Reader"},
		AllowedGroups: []string{"staff"},
	}}
}

func TestOIDCSignIn(t *testing.T) {
	s := newSSOServer(t, ssoConfig())
	s.issuer.Set(func(is *oidctest.Issuer) { is.User = ssoUser("kc-1", "jdoe", "/staff", "/it/devops", "devops") })

	resp, body := s.client(t).do(http.MethodGet, "/auth/providers", nil)
	expect(t, resp, body, http.StatusOK, "")
	if o, _ := body["oidc"].(map[string]any); o["display_name"] != "Acme SSO" || o["start_url"] != "/api/v1/auth/oidc/start" {
		t.Fatalf("providers = %v", body)
	}

	c := s.client(t)
	if loc := c.sso("/repositories?tab=all"); loc.Path != "/repositories" || loc.RawQuery != "tab=all" {
		t.Fatalf("landed on %s", loc)
	}
	resp, body = c.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	user := body["user"].(map[string]any)
	if user["username"] != "jdoe" || user["source"] != "oidc" || body["auth_method"] != "session" {
		t.Fatalf("session = %v", body)
	}
	if roles := user["roles"].([]any); !slices.Equal(roles, []any{"Publisher", "Reader"}) {
		t.Fatalf("roles = %v", roles)
	}
	// The session is a normal one: mutating requests need the CSRF token.
	c.csrf = resp.Header.Get("X-CSRF-Token")
	resp, body = c.do(http.MethodPost, "/auth/logout", nil)
	expect(t, resp, body, http.StatusNoContent, "")

	if ev := s.env.Audit.Events(auth.AuditLogin); len(ev) != 1 || ev[0].Details["method"] != auth.MethodOIDC {
		t.Fatalf("login audit = %+v", ev)
	}

	// Next sign-in without the devops group drops the mapped role.
	s.issuer.Set(func(is *oidctest.Issuer) { is.User = ssoUser("kc-1", "jdoe", "staff") })
	c2 := s.client(t)
	if loc := c2.sso("/"); loc.Path != "/" {
		t.Fatalf("landed on %s", loc)
	}
	_, body = c2.do(http.MethodGet, "/auth/session", nil)
	if roles := body["user"].(map[string]any)["roles"].([]any); !slices.Equal(roles, []any{"Reader"}) {
		t.Fatalf("roles after re-sync = %v", roles)
	}
}

func TestOIDCSignInFailures(t *testing.T) {
	s := newSSOServer(t, ssoConfig())
	admin := s.env.Admin(t)
	if _, err := s.env.Svc.CreateUser(admin, auth.UserInput{Username: "localbob", Kind: auth.KindUser, Password: ptr(pw)}); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		set  func(is *oidctest.Issuer)
		code string
	}{
		"group not allowed": {func(is *oidctest.Issuer) { is.User = ssoUser("kc-2", "guest", "contractors") }, auth.CodeGroupDenied},
		"local username":    {func(is *oidctest.Issuer) { is.User = ssoUser("kc-3", "localbob", "staff") }, auth.CodeAccountConflict},
		"provider refused":  {func(is *oidctest.Issuer) { is.Deny = true }, oidc.CodeDenied},
		"wrong audience": {func(is *oidctest.Issuer) {
			is.User, is.Mutate = ssoUser("kc-4", "eve", "staff"), func(c map[string]any) { c["aud"] = "other" }
		}, oidc.CodeFailed},
		"bad nonce": {func(is *oidctest.Issuer) {
			is.User, is.Mutate = ssoUser("kc-5", "eve", "staff"), func(c map[string]any) { c["nonce"] = "x" }
		}, oidc.CodeFailed},
	} {
		t.Run(name, func(t *testing.T) {
			s.issuer.Set(func(is *oidctest.Issuer) {
				is.Deny, is.Mutate = false, nil
				tc.set(is)
			})
			c := s.client(t)
			loc := c.sso("/")
			if loc.Path != "/login" || loc.Query().Get("error") != tc.code {
				t.Fatalf("landed on %s, want /login?error=%s", loc, tc.code)
			}
			resp, body := c.do(http.MethodGet, "/auth/session", nil)
			expect(t, resp, body, http.StatusUnauthorized, "")
		})
	}
	if u, _ := s.env.Svc.GetUserByName(context.Background(), "localbob"); u.Source != auth.SourceLocal {
		t.Fatalf("local user was taken over: %+v", u)
	}
	if n := len(s.env.Audit.Events(auth.AuditLoginFailed)); n < 5 {
		t.Fatalf("failed sign-ins audited %d times, want >= 5", n)
	}
}

func TestOIDCCallbackRejectsForgedState(t *testing.T) {
	s := newSSOServer(t, ssoConfig())
	s.issuer.Set(func(is *oidctest.Issuer) { is.User = ssoUser("kc-1", "jdoe", "staff") })
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Transport: s.Client().Transport, Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Start a sign-in so the browser holds a state cookie.
	resp, err := hc.Get(s.URL + "/api/v1/auth/oidc/start")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	// A callback with another state (login CSRF) is refused.
	resp, err = hc.Get(s.URL + "/api/v1/auth/oidc/callback?code=stolen&state=attacker")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/login?error="+oidc.CodeState {
		t.Fatalf("location = %q", loc)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "hangar_session" && ck.Value != "" {
			t.Fatal("session started on a forged state")
		}
	}
}

func TestOIDCStateCookie(t *testing.T) {
	s := newSSOServer(t, ssoConfig())
	hc := &http.Client{Transport: s.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(s.URL + "/api/v1/auth/oidc/start?redirect=//evil.example.com")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var state *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == oidc.CookieName {
			state = ck
		}
	}
	if state == nil || !state.HttpOnly || !state.Secure || state.SameSite != http.SameSiteLaxMode ||
		state.Path != oidc.CookiePath || state.MaxAge <= 0 || state.MaxAge > 600 {
		t.Fatalf("state cookie = %+v", state)
	}
	if strings.Contains(state.Value, "evil") {
		t.Fatal("state cookie is not encrypted")
	}
	loc, _ := resp.Location()
	if !strings.HasPrefix(loc.String(), s.issuer.URL+"/authorize?") {
		t.Fatalf("start redirected to %s", loc)
	}
}

func TestOIDCDisabled(t *testing.T) {
	s := newServer(t, auth.Config{})
	resp, body := s.client(t).do(http.MethodGet, "/auth/oidc/start", nil)
	expect(t, resp, body, http.StatusNotFound, "not_found")
}

func TestIdentityProviders(t *testing.T) {
	s := newSSOServer(t, ssoConfig())
	c := s.client(t)
	resp, body := c.do(http.MethodGet, "/identity-providers", nil)
	expect(t, resp, body, http.StatusUnauthorized, "")
	s.env.Admin(t)
	resp, body = c.login("admin", authtest.AdminPassword, false)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = c.do(http.MethodGet, "/identity-providers", nil)
	expect(t, resp, body, http.StatusOK, "")
	items := body["items"].([]any)
	o, l := items[0].(map[string]any), items[1].(map[string]any)
	if o["name"] != "oidc" || o["enabled"] != true || l["name"] != "ldap" || l["state"] != "disabled" {
		t.Fatalf("providers = %v", items)
	}
}

// stubDirectory accepts one directory user.
type stubDirectory struct{}

func (stubDirectory) Authenticate(_ context.Context, username, password string) (*auth.ExternalIdentity, error) {
	if (username != "jdoe" && username != "admin") || password != "directory password" {
		return nil, core.ErrUnauthenticated
	}
	return &auth.ExternalIdentity{Source: auth.SourceLDAP, Username: username, Groups: []string{"Hangar Publishers"}}, nil
}

func TestLDAPRealmLogin(t *testing.T) {
	s := newServer(t, auth.Config{LDAP: auth.LDAPConfig{
		Enabled: true, URL: "ldaps://dc1.example.com", UserBaseDN: "DC=example,DC=com", AutoCreate: true,
		GroupRoles: map[string][]string{"Hangar Publishers": {"Publisher"}},
	}}, authtest.WithDirectory(stubDirectory{}))
	s.env.Admin(t)
	c := s.client(t)
	_, body := c.do(http.MethodGet, "/auth/providers", nil)
	if body["ldap"].(map[string]any)["enabled"] != true {
		t.Fatalf("providers = %v", body)
	}
	login := func(user, pass, realm string) (*http.Response, map[string]any) {
		return c.do(http.MethodPost, "/auth/login", map[string]any{"username": user, "password": pass, "remember": false, "realm": realm})
	}
	resp, body := login("jdoe", "wrong", "ldap")
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")
	resp, body = login("jdoe", "directory password", "local")
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")
	resp, body = login("admin", "directory password", "ldap")
	expect(t, resp, body, http.StatusConflict, "conflict")
	resp, body = login("jdoe", "directory password", "ldap")
	expect(t, resp, body, http.StatusOK, "")
	user := body["user"].(map[string]any)
	if user["source"] != "ldap" || !slices.Equal(user["roles"].([]any), []any{"Publisher"}) {
		t.Fatalf("session = %v", body)
	}
	// Package clients use the same directory password with Basic auth.
	req, _ := http.NewRequest(http.MethodGet, s.URL+"/api/v1/auth/session", nil)
	req.SetBasicAuth("jdoe", "directory password")
	r2, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("basic auth status %d", r2.StatusCode)
	}
}

func TestLDAPRealmDisabled(t *testing.T) {
	s := newServer(t, auth.Config{})
	resp, body := s.client(t).do(http.MethodPost, "/auth/login", map[string]any{"username": "a", "password": "b", "remember": false, "realm": "ldap"})
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
}

func ptr[T any](v T) *T { return &v }
