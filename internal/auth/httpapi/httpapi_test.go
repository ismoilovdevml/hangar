package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/auth/httpapi"
	"github.com/ismoilovdevml/hangar/internal/core"
)

const pw = "a long enough passphrase"

type testServer struct {
	*httptest.Server
	env *authtest.Env
}

func newServer(t *testing.T, cfg auth.Config) *testServer {
	t.Helper()
	env := authtest.New(t, cfg)
	r := chi.NewRouter()
	r.Mount("/api/v1", httpapi.Routes(env.Svc, httpapi.Options{
		Organization: httpapi.Organization{Slug: "acme", Name: "Acme", Environment: "test", Region: "local"},
	}))
	srv := httptest.NewTLSServer(r)
	t.Cleanup(srv.Close)
	return &testServer{Server: srv, env: env}
}

// client is a browser-like client: cookie jar, Origin header, CSRF echo.
type client struct {
	t      *testing.T
	srv    *testServer
	hc     *http.Client
	csrf   string
	header http.Header
}

func (s *testServer) client(t *testing.T) *client {
	jar, _ := cookiejar.New(nil)
	// Server.Client() is shared; each browser needs its own cookie jar.
	hc := &http.Client{Transport: s.Client().Transport, Jar: jar}
	return &client{t: t, srv: s, hc: hc, header: http.Header{}}
}

func (c *client) do(method, path string, body any) (*http.Response, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+"/api/v1"+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", c.srv.URL)
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	for k, v := range c.header {
		req.Header[k] = v
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s: non-JSON body %q", method, path, raw)
		}
	}
	return resp, out
}

func (c *client) login(user, pass string, remember bool) (*http.Response, map[string]any) {
	c.t.Helper()
	resp, body := c.do(http.MethodPost, "/auth/login", map[string]any{"username": user, "password": pass, "remember": remember, "realm": "local"})
	if resp.StatusCode == http.StatusOK {
		c.csrf = resp.Header.Get("X-CSRF-Token")
	}
	return resp, body
}

func expect(t *testing.T, resp *http.Response, body map[string]any, status int, code string) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d (body %v)", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, status, body)
	}
	if code != "" && body["code"] != code {
		t.Fatalf("%s %s: code %v, want %s (body %v)", resp.Request.Method, resp.Request.URL.Path, body["code"], code, body)
	}
}

func TestProviders(t *testing.T) {
	s := newServer(t, auth.Config{})
	resp, body := s.client(t).do(http.MethodGet, "/auth/providers", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["oidc"] != nil || body["local"].(map[string]any)["enabled"] != true ||
		body["ldap"].(map[string]any)["enabled"] != false || body["saml"].(map[string]any)["enabled"] != false {
		t.Fatalf("providers = %v", body)
	}
}

func TestLoginSessionAndCSRF(t *testing.T) {
	s := newServer(t, auth.Config{})
	s.env.Admin(t)
	c := s.client(t)

	resp, body := c.login("admin", "wrong password!!", false)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")
	resp, body = c.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")

	resp, body = c.login("admin", authtest.AdminPassword, false)
	expect(t, resp, body, http.StatusOK, "")
	user := body["user"].(map[string]any)
	if user["username"] != "admin" || body["organization"].(map[string]any)["slug"] != "acme" || len(body["privileges"].([]any)) == 0 {
		t.Fatalf("session body = %v", body)
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, ck := range resp.Cookies() {
		switch ck.Name {
		case "hangar_session":
			sessionCookie = ck
		case "hangar_csrf":
			csrfCookie = ck
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.MaxAge != 0 {
		t.Fatalf("session cookie = %+v", sessionCookie)
	}
	if csrfCookie == nil || csrfCookie.HttpOnly || !csrfCookie.Secure || csrfCookie.Value != c.csrf {
		t.Fatalf("csrf cookie = %+v (header %q)", csrfCookie, c.csrf)
	}

	resp, body = c.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	if resp.Header.Get("X-CSRF-Token") != c.csrf {
		t.Fatal("session endpoint does not return the CSRF token")
	}

	// Mutations with the cookie need the CSRF header and a same-origin Origin.
	tokenReq := map[string]any{"name": "ci", "scopes": []string{"repo:*:read"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	good := c.csrf
	c.csrf = ""
	resp, body = c.do(http.MethodPost, "/tokens", tokenReq)
	expect(t, resp, body, http.StatusForbidden, "csrf_failed")
	c.csrf = "forged"
	resp, body = c.do(http.MethodPost, "/tokens", tokenReq)
	expect(t, resp, body, http.StatusForbidden, "csrf_failed")
	c.csrf = good
	c.header.Set("Origin", "https://evil.example")
	resp, body = c.do(http.MethodPost, "/tokens", tokenReq)
	expect(t, resp, body, http.StatusForbidden, "forbidden")
	c.header.Del("Origin")
	resp, body = c.do(http.MethodPost, "/tokens", tokenReq)
	expect(t, resp, body, http.StatusCreated, "")
	secret, _ := body["secret"].(string)
	if !strings.HasPrefix(secret, core.TokenPrefix) || body["owner"] != "admin" || body["prefix"] != secret[:8] {
		t.Fatalf("token created = %v", body)
	}

	// A bearer token needs no CSRF token and works without cookies.
	tc := s.client(t)
	tc.header.Set("Authorization", "Bearer "+secret)
	resp, body = tc.do(http.MethodGet, "/tokens", nil)
	expect(t, resp, body, http.StatusOK, "")
	items := body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["secret"] != nil {
		t.Fatalf("tokens = %v", body)
	}
	if _, ok := body["next_cursor"]; !ok || body["next_cursor"] != nil {
		t.Fatalf("next_cursor = %v, want null", body["next_cursor"])
	}
	// The token is scoped to repo:*:read, so it cannot manage users.
	resp, body = tc.do(http.MethodGet, "/users", nil)
	expect(t, resp, body, http.StatusForbidden, "forbidden")
	// Basic with the token as the password.
	bc := s.client(t)
	bc.header.Set("Authorization", "Basic "+basic("anything", secret))
	resp, body = bc.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	bad := s.client(t)
	bad.header.Set("Authorization", "Bearer hgr_bogus")
	resp, body = bad.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")

	// Revoke through the cookie session.
	id := items[0].(map[string]any)["id"].(string)
	resp, body = c.do(http.MethodDelete, "/tokens/"+id, nil)
	expect(t, resp, body, http.StatusNoContent, "")
	resp, body = tc.do(http.MethodGet, "/tokens", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")

	// Logout ends the session.
	resp, body = c.do(http.MethodPost, "/auth/logout", nil)
	expect(t, resp, body, http.StatusNoContent, "")
	resp, body = c.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")

	if n := len(s.env.Audit.Events(auth.AuditLogin)); n != 1 {
		t.Errorf("successful logins audited = %d", n)
	}
	if n := len(s.env.Audit.Events(auth.AuditLoginFailed)); n != 1 {
		t.Errorf("failed logins audited = %d", n)
	}
	if n := len(s.env.Audit.Events(auth.AuditLogout)); n != 1 {
		t.Errorf("logouts audited = %d", n)
	}
}

func basic(u, p string) string {
	req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
	req.SetBasicAuth(u, p)
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Basic ")
}

func TestRememberMe(t *testing.T) {
	s := newServer(t, auth.Config{})
	s.env.Admin(t)
	resp, body := s.client(t).login("admin", authtest.AdminPassword, true)
	expect(t, resp, body, http.StatusOK, "")
	for _, ck := range resp.Cookies() {
		if ck.Name == "hangar_session" && ck.MaxAge < 29*24*3600 {
			t.Fatalf("remember-me cookie MaxAge = %d", ck.MaxAge)
		}
	}
}

func TestLoginLockoutAndRateLimit(t *testing.T) {
	s := newServer(t, auth.Config{
		Lockout:   auth.Lockout{MaxAttempts: 2, Window: time.Minute, Duration: 5 * time.Minute},
		LoginRate: auth.RateLimit{PerMinute: 1, Burst: 4},
	})
	s.env.Admin(t)
	c := s.client(t)
	resp, body := c.login("admin", "wrong password!!", false)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")
	resp, body = c.login("admin", "wrong password!!", false)
	expect(t, resp, body, http.StatusLocked, "locked")
	if ra, _ := body["retry_after"].(float64); ra < 290 || ra > 300 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("retry_after = %v, header %q", body["retry_after"], resp.Header.Get("Retry-After"))
	}
	resp, body = c.login("admin", authtest.AdminPassword, false)
	expect(t, resp, body, http.StatusLocked, "locked")
	resp, body = c.login("admin", authtest.AdminPassword, false)
	expect(t, resp, body, http.StatusLocked, "locked")
	// Fifth attempt from this IP within the burst window is rate limited.
	resp, body = c.login("admin", authtest.AdminPassword, false)
	expect(t, resp, body, http.StatusTooManyRequests, "rate_limited")
	if resp.Header.Get("Retry-After") == "" || body["retry_after"] == nil {
		t.Fatalf("429 without retry_after: %v", body)
	}
}

func TestLoginValidation(t *testing.T) {
	s := newServer(t, auth.Config{})
	c := s.client(t)
	resp, body := c.do(http.MethodPost, "/auth/login", map[string]any{"username": "a", "password": "b", "realm": "ldap"})
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
	resp, body = c.do(http.MethodPost, "/auth/login", map[string]any{"username": "", "password": "b"})
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")

	// A form post (what a cross-site page can send without CORS) is refused.
	req, _ := http.NewRequest(http.MethodPost, s.URL+"/api/v1/auth/login", strings.NewReader(url.Values{"username": {"a"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("form login status = %d", r.StatusCode)
	}
	c.header.Set("Origin", "https://evil.example")
	resp, body = c.do(http.MethodPost, "/auth/login", map[string]any{"username": "a", "password": "b"})
	expect(t, resp, body, http.StatusForbidden, "forbidden")
}

func TestUsersAPI(t *testing.T) {
	s := newServer(t, auth.Config{})
	s.env.Admin(t)
	c := s.client(t)
	resp, body := c.login("admin", authtest.AdminPassword, false)
	expect(t, resp, body, http.StatusOK, "")

	resp, body = c.do(http.MethodPost, "/users", map[string]any{"username": "olga", "kind": "user", "password": pw, "roles": []string{"Reader"}, "email": "olga@example.test"})
	expect(t, resp, body, http.StatusCreated, "")
	id := body["id"].(string)
	if body["source"] != "local" || body["email"] != "olga@example.test" || body["token_count"] != float64(0) || body["avatar_url"] != nil {
		t.Fatalf("member = %v", body)
	}
	resp, body = c.do(http.MethodPost, "/users", map[string]any{"username": "olga", "kind": "user"})
	expect(t, resp, body, http.StatusConflict, "conflict")
	resp, body = c.do(http.MethodPost, "/users", map[string]any{"username": "weak", "kind": "user", "password": "123"})
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
	resp, body = c.do(http.MethodPost, "/users", map[string]any{"username": "svc-ci", "kind": "service_account"})
	expect(t, resp, body, http.StatusCreated, "")
	if body["email"] != nil {
		t.Fatalf("empty email should be null: %v", body)
	}

	resp, body = c.do(http.MethodGet, "/users?kind=service_account", nil)
	expect(t, resp, body, http.StatusOK, "")
	if items := body["items"].([]any); len(items) != 1 || items[0].(map[string]any)["username"] != "svc-ci" {
		t.Fatalf("filtered list = %v", body)
	}
	resp, body = c.do(http.MethodGet, "/users?limit=1", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["next_cursor"] == nil {
		t.Fatalf("expected a next page: %v", body)
	}
	resp, body = c.do(http.MethodGet, "/users?limit=0", nil)
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")

	resp, body = c.do(http.MethodPut, "/users/"+id, map[string]any{"username": "olga", "kind": "user", "display_name": "Olga", "roles": []string{"Publisher"}})
	expect(t, resp, body, http.StatusOK, "")
	if body["display_name"] != "Olga" || body["roles"].([]any)[0] != "Publisher" || body["email"] != "olga@example.test" {
		t.Fatalf("updated = %v", body)
	}
	resp, body = c.do(http.MethodGet, "/users/"+id, nil)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = c.do(http.MethodGet, "/users/not-a-uuid", nil)
	expect(t, resp, body, http.StatusNotFound, "not_found")

	// Olga is not an administrator.
	oc := s.client(t)
	resp, body = oc.login("olga", pw, false)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = oc.do(http.MethodGet, "/users", nil)
	expect(t, resp, body, http.StatusForbidden, "forbidden")
	resp, body = oc.do(http.MethodPost, "/roles", map[string]any{"name": "mine", "privileges": []string{"system:*"}})
	expect(t, resp, body, http.StatusForbidden, "forbidden")

	// Roles.
	resp, body = c.do(http.MethodGet, "/roles", nil)
	expect(t, resp, body, http.StatusOK, "")
	if n := len(body["items"].([]any)); n != 4 {
		t.Fatalf("roles = %d", n)
	}
	resp, body = c.do(http.MethodPost, "/roles", map[string]any{"name": "auditors", "privileges": []string{"system:audit:read"}})
	expect(t, resp, body, http.StatusCreated, "")
	resp, body = c.do(http.MethodPost, "/roles", map[string]any{"name": "bad", "privileges": []string{"repo:*:fly"}})
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")
	resp, body = c.do(http.MethodPut, "/roles/Admin", map[string]any{"name": "Admin", "privileges": []string{}})
	expect(t, resp, body, http.StatusConflict, "conflict")
	resp, body = c.do(http.MethodDelete, "/roles/auditors", nil)
	expect(t, resp, body, http.StatusNoContent, "")

	resp, body = c.do(http.MethodDelete, "/users/"+id, nil)
	expect(t, resp, body, http.StatusNoContent, "")
	// Olga's session ended with her account.
	resp, body = oc.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")
}
