package httpapi_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/auth/httpapi"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// newFullServer serves the auth routes next to stand-ins for other API
// routes, all behind the authentication middleware as in production.
func newFullServer(t *testing.T, opts ...authtest.Option) *testServer {
	t.Helper()
	env := authtest.New(t, auth.Config{}, opts...)
	whoami := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"principal":"` + core.PrincipalFrom(r.Context()).Name + `"}`))
	}
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		o := httpapi.Options{Organization: httpapi.Organization{Slug: "acme", Name: "Acme"}}
		r.Use(httpapi.Middleware(env.Svc, o))
		httpapi.Register(r, env.Svc, o)
		r.Get("/repositories", whoami)
		r.Delete("/repositories/{name}", whoami)
		r.Get("/system/logs", whoami)
		r.Get("/system/status", whoami)
	})
	srv := httptest.NewTLSServer(r)
	t.Cleanup(srv.Close)
	return &testServer{Server: srv, env: env}
}

func sessionCookie(t *testing.T, c *client) string {
	t.Helper()
	for _, ck := range c.hc.Jar.Cookies(mustURL(t, c.srv.URL)) {
		if ck.Name == "hangar_session" {
			return ck.Value
		}
	}
	return ""
}

func TestFirstSignInPasswordChange(t *testing.T) {
	file := filepath.Join(t.TempDir(), "initial-admin-password")
	s := newFullServer(t, authtest.WithInitialPasswordFile(file))
	a, err := s.env.Svc.EnsureInitialAdmin(context.Background())
	if err != nil || a == nil {
		t.Fatalf("initial admin: %+v, %v", a, err)
	}
	initial := a.Password

	c := s.client(t)
	resp, body := c.login("admin", initial, true)
	expect(t, resp, body, http.StatusOK, "")
	if body["password_change_required"] != true || len(body["privileges"].([]any)) != 0 {
		t.Fatalf("login body = %v", body)
	}
	if pol := body["password_policy"].(map[string]any); pol["min_length"] != float64(12) {
		t.Fatalf("password policy = %v", pol)
	}
	resp, body = c.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["password_change_required"] != true {
		t.Fatalf("session = %v", body)
	}

	// Everything else is refused with a dedicated code.
	id := body["user"].(map[string]any)["id"].(string)
	for _, rt := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/users", nil},
		{http.MethodGet, "/users/" + id, nil},
		{http.MethodPost, "/users/" + id + "/require-password-change", nil},
		{http.MethodGet, "/tokens", nil},
		{http.MethodPost, "/tokens", map[string]any{"name": "ci", "scopes": []string{"repo:*:read"}}},
		{http.MethodPut, "/me/profile", map[string]any{"display_name": "Root"}},
		{http.MethodGet, "/me/sessions", nil},
		{http.MethodDelete, "/me/sessions", nil},
		{http.MethodGet, "/roles", nil},
		{http.MethodGet, "/repositories", nil},
		{http.MethodDelete, "/repositories/npm-hosted", nil},
		{http.MethodGet, "/system/logs", nil},
		{http.MethodGet, "/no/such/route", nil},
	} {
		resp, body := c.do(rt.method, rt.path, rt.body)
		expect(t, resp, body, http.StatusForbidden, "password_change_required")
	}
	// Public endpoints answer as for a visitor.
	resp, body = c.do(http.MethodGet, "/auth/providers", nil)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = c.do(http.MethodGet, "/system/status", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["principal"] != core.AnonymousName {
		t.Fatalf("public route principal = %v", body["principal"])
	}

	// HTTP Basic with the password is limited the same way.
	basic := s.client(t)
	basic.header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:"+initial)))
	resp, body = basic.do(http.MethodGet, "/users", nil)
	expect(t, resp, body, http.StatusForbidden, "password_change_required")
	resp, body = basic.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["password_change_required"] != true || body["auth_method"] != "basic" {
		t.Fatalf("basic session = %v", body)
	}

	// Keeping the given password is refused.
	resp, body = c.do(http.MethodPut, "/me/password", map[string]any{"current_password": initial, "new_password": initial})
	expect(t, resp, body, http.StatusBadRequest, "invalid_argument")

	// A successful change re-issues the session and lifts the limits.
	oldCookie, oldCSRF := sessionCookie(t, c), c.csrf
	resp, body = c.do(http.MethodPut, "/me/password", map[string]any{"current_password": initial, "new_password": "a private passphrase of mine"})
	expect(t, resp, body, http.StatusNoContent, "")
	c.csrf = resp.Header.Get("X-CSRF-Token")
	if newCookie := sessionCookie(t, c); newCookie == "" || newCookie == oldCookie || c.csrf == "" || c.csrf == oldCSRF {
		t.Fatal("session not re-issued after the password change")
	}
	resp, body = c.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["password_change_required"] != false || len(body["privileges"].([]any)) == 0 {
		t.Fatalf("session after the change = %v", body)
	}
	resp, body = c.do(http.MethodGet, "/users", nil)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = c.do(http.MethodGet, "/repositories", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["principal"] != "admin" {
		t.Fatalf("principal = %v", body["principal"])
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("initial password file still there: %v", err)
	}

	// The old session cookie is dead.
	stale := s.client(t)
	stale.hc.Jar.SetCookies(mustURL(t, s.URL), []*http.Cookie{{Name: "hangar_session", Value: oldCookie, Path: "/"}})
	resp, body = stale.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")

	ev := s.env.Audit.Events(auth.AuditUserPasswordChanged)
	if len(ev) != 1 || ev[0].Details["reason"] != "first_login" {
		t.Fatalf("audit = %+v", ev)
	}
}

func TestRestrictedSessionCanSignOut(t *testing.T) {
	s := newFullServer(t)
	a, err := s.env.Svc.EnsureInitialAdmin(context.Background())
	if err != nil || a == nil {
		t.Fatalf("initial admin: %+v, %v", a, err)
	}
	c := s.client(t)
	resp, body := c.login("admin", a.Password, false)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = c.do(http.MethodPost, "/auth/logout", nil)
	expect(t, resp, body, http.StatusNoContent, "")
	resp, body = c.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusUnauthorized, "unauthenticated")
}

func TestRequirePasswordChangeEndpoint(t *testing.T) {
	s := newFullServer(t)
	admin := s.env.Admin(t)
	alice, err := s.env.Svc.CreateUser(admin, auth.UserInput{Username: "alice", Kind: auth.KindUser, Password: &[]string{pw}[0], Roles: []string{"Reader"}})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.env.Svc.CreateUser(admin, auth.UserInput{Username: "bob", Kind: auth.KindUser, Password: &[]string{pw}[0]})
	if err != nil {
		t.Fatal(err)
	}

	reader := s.client(t)
	resp, body := reader.login("alice", pw, false)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = reader.do(http.MethodPost, "/users/"+bob.ID+"/require-password-change", nil)
	expect(t, resp, body, http.StatusForbidden, "forbidden")

	ac := s.client(t)
	resp, body = ac.login("admin", authtest.AdminPassword, false)
	expect(t, resp, body, http.StatusOK, "")
	resp, body = ac.do(http.MethodPost, "/users/"+alice.ID+"/require-password-change", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["must_change_password"] != true {
		t.Fatalf("member = %v", body)
	}
	resp, body = ac.do(http.MethodGet, "/users/"+alice.ID, nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["must_change_password"] != true {
		t.Fatalf("member = %v", body)
	}
	// Alice's open session is limited from her next request on.
	resp, body = reader.do(http.MethodGet, "/repositories", nil)
	expect(t, resp, body, http.StatusForbidden, "password_change_required")
	resp, body = reader.do(http.MethodGet, "/auth/session", nil)
	expect(t, resp, body, http.StatusOK, "")
	if body["password_change_required"] != true {
		t.Fatalf("alice session = %v", body)
	}
	if ev := s.env.Audit.Events(auth.AuditUserPasswordRequire); len(ev) != 2 || ev[1].Actor != "admin" {
		t.Fatalf("audit = %+v", ev)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
