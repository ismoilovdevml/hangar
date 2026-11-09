package auth_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// TestReloadPrincipal: a principal that authenticated earlier is rebuilt from
// the current account, so disabling the user, revoking the token or changing
// roles takes effect.
func TestReloadPrincipal(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	ctx := context.Background()
	u := mustCreateUser(t, e, admin, "frank", "Reader")
	byPassword := principalOf(t, e, "frank")

	tokCtx := core.WithPrincipal(ctx, byPassword)
	tok, secret, err := e.Svc.CreateToken(tokCtx, auth.TokenInput{Name: "ci", Scopes: privs("repo:*:read"), ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	byToken, err := e.Svc.Authenticate(ctx, core.Credentials{Token: secret})
	if err != nil {
		t.Fatal(err)
	}

	if p, err := e.Svc.ReloadPrincipal(ctx, core.Anonymous()); err != nil || !p.IsAnonymous() {
		t.Fatalf("anonymous = %+v, %v", p, err)
	}
	// Current roles are returned, not the ones the principal carried.
	if err := e.Svc.BindRole(admin, "Publisher", u.ID); err != nil {
		t.Fatal(err)
	}
	p, err := e.Svc.ReloadPrincipal(ctx, byPassword)
	if err != nil || !slices.Equal(p.Roles, []string{"Publisher", "Reader"}) || p.TokenID != "" {
		t.Fatalf("reloaded = %+v, %v", p, err)
	}
	p, err = e.Svc.ReloadPrincipal(ctx, byToken)
	if err != nil || p.TokenID != tok.ID || len(p.Scopes) != 1 || p.Scopes[0].String() != "repo:*:read" {
		t.Fatalf("reloaded token principal = %+v, %v", p, err)
	}

	// A forged principal for an unknown account is refused.
	forged := *byPassword
	forged.ID = "00000000-0000-0000-0000-000000000000"
	if _, err := e.Svc.ReloadPrincipal(ctx, &forged); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("unknown account: %v", err)
	}

	// A revoked token, and then a disabled account, are refused.
	if err := e.Svc.RevokeToken(tokCtx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.ReloadPrincipal(ctx, byToken); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("revoked token: %v", err)
	}
	if _, err := e.Svc.SetUserDisabled(admin, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.ReloadPrincipal(ctx, byPassword); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("disabled user: %v", err)
	}
	refused := 0
	for _, ev := range e.Audit.Events(auth.AuditTokenAuthFailed) {
		if ev.Details["method"] == auth.MethodRenew {
			refused++
		}
	}
	if refused != 3 {
		t.Errorf("audited refusals = %d, want 3", refused)
	}
}
