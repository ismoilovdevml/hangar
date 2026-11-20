package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/auth/httpapi"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// Revalidate (used by the event stream) fails once the request's session
// or token no longer works, and returns the current principal before that.
func TestRevalidate(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	if _, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "val", Kind: auth.KindUser, Password: ptrString(pw), Roles: []string{"Reader"}}); err != nil {
		t.Fatal(err)
	}
	val := e.As(t, "val", pw)
	mw := httpapi.Middleware(e.Svc, httpapi.Options{})
	revalidate := httpapi.Revalidate(e.Svc)

	// requestContext runs r through the middleware and returns the context
	// a handler (the event stream) would see.
	requestContext := func(r *http.Request) context.Context {
		t.Helper()
		var got context.Context
		rec := httptest.NewRecorder()
		mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() })).ServeHTTP(rec, r)
		if got == nil {
			t.Fatalf("middleware refused the request: %d %s", rec.Code, rec.Body)
		}
		return got
	}

	// Browser session: ends with sign-out.
	secret, _, err := e.Svc.CreateSession(val, core.PrincipalFrom(val), false)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.AddCookie(&http.Cookie{Name: "hangar_session", Value: secret})
	ctx := requestContext(r)
	if p, err := revalidate(ctx); err != nil || p.Name != "val" {
		t.Fatalf("live session: %+v, %v", p, err)
	}
	if err := e.Svc.DeleteSession(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := revalidate(ctx); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("after sign-out: %v, want unauthenticated", err)
	}

	// Token: ends with revocation.
	tok, tokSecret, err := e.Svc.CreateToken(val, auth.TokenInput{Name: "stream", Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:read")},
		ExpiresAt: e.Clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.Header.Set("Authorization", "Bearer "+tokSecret)
	ctx = requestContext(r)
	if p, err := revalidate(ctx); err != nil || p.TokenID != tok.ID {
		t.Fatalf("live token: %+v, %v", p, err)
	}
	if err := e.Svc.RevokeToken(val, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := revalidate(ctx); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("after revocation: %v, want unauthenticated", err)
	}
}

func ptrString(s string) *string { return &s }
