package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// RevokeIssued takes back a session token Issue minted for the caller (a
// client logout), and nothing else: not a personal access token, not
// another user's session token.
func TestRevokeIssued(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "ivy", "Publisher")
	mustCreateUser(t, e, admin, "jack", "Publisher")
	ivy, jack := e.As(t, "ivy", pw), e.As(t, "jack", pw)
	ctx := context.Background()

	secret, _, err := e.Svc.Issue(ctx, core.PrincipalFrom(ivy), core.TokenRequest{Name: "npm login (npm-hosted)", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.Svc.Authenticate(ctx, core.Credentials{Token: secret})
	if err != nil {
		t.Fatal(err)
	}
	pat, patSecret, err := e.Svc.CreateToken(ivy, auth.TokenInput{Name: "ci", Scopes: []core.Privilege{core.MustParsePrivilege("repo:npm-hosted:read")}, ExpiresAt: e.Clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	if err := e.Svc.RevokeIssued(ctx, p.TokenID); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("anonymous revoke = %v", err)
	}
	if err := e.Svc.RevokeIssued(jack, p.TokenID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("another user's revoke = %v", err)
	}
	if err := e.Svc.RevokeIssued(ivy, pat.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("personal token revoke = %v", err)
	}
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Token: patSecret}); err != nil {
		t.Fatalf("personal token stopped working: %v", err)
	}
	if err := e.Svc.RevokeIssued(core.WithPrincipal(ctx, p), p.TokenID); err != nil {
		t.Fatalf("revoke own session token: %v", err)
	}
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Token: secret}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("revoked session token accepted: %v", err)
	}
	if err := e.Svc.RevokeIssued(ivy, p.TokenID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second revoke = %v", err)
	}
	if ev := e.Audit.Events(auth.AuditTokenRevoke); len(ev) != 1 || ev[0].Details["kind"] != "session" {
		t.Fatalf("revoke audit = %+v", ev)
	}
}
