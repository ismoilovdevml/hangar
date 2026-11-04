package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/auth/httpapi"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// Package routes accept the web UI's session cookie for downloads only, and
// never at the expense of client credentials.
func TestDownloadSessions(t *testing.T) {
	env := authtest.New(t, auth.Config{})
	env.Admin(t)
	p, err := env.Svc.Login(context.Background(), "admin", authtest.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := env.Svc.CreateSession(context.Background(), p, false)
	if err != nil {
		t.Fatal(err)
	}
	authn := httpapi.SessionAuthenticator(env.Svc)
	h := httpapi.DownloadSessions(env.Svc)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := authn.Authenticate(r.Context(), core.CredentialsFromRequest(r))
		if err != nil {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, p.Name)
	}))
	cookie := &http.Cookie{Name: env.Svc.Config().Session.CookieName, Value: secret}
	call := func(method string, c *http.Cookie, basic bool) (int, string) {
		req := httptest.NewRequest(method, "/repository/raw/x", nil)
		if c != nil {
			req.AddCookie(c)
		}
		if basic {
			req.SetBasicAuth("admin", "wrong password!!")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, body := call(http.MethodGet, cookie, false); code != 200 || body != "admin" {
		t.Errorf("GET with session = %d %q, want admin", code, body)
	}
	if code, body := call(http.MethodHead, cookie, false); code != 200 {
		t.Errorf("HEAD with session = %d %q", code, body)
	}
	if code, body := call(http.MethodPut, cookie, false); code != 200 || body != core.AnonymousName {
		t.Errorf("PUT with session = %d %q, want anonymous (cookies never write)", code, body)
	}
	if code, _ := call(http.MethodGet, cookie, true); code != http.StatusUnauthorized {
		t.Errorf("GET with session + bad basic = %d, want 401 (credentials win)", code)
	}
	if code, body := call(http.MethodGet, &http.Cookie{Name: cookie.Name, Value: "bogus"}, false); code != 200 || body != core.AnonymousName {
		t.Errorf("GET with unknown session = %d %q, want anonymous", code, body)
	}
	if code, body := call(http.MethodGet, nil, false); code != 200 || body != core.AnonymousName {
		t.Errorf("GET without cookie = %d %q", code, body)
	}
}
