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

func privs(ss ...string) []core.Privilege {
	out := make([]core.Privilege, 0, len(ss))
	for _, s := range ss {
		out = append(out, core.MustParsePrivilege(s))
	}
	return out
}

func from(ip string) context.Context {
	return core.WithRequestInfo(context.Background(), core.RequestInfo{RemoteIP: ip})
}

// A token never reaches beyond its owner's roles, whatever its scopes say.
func TestTokenScopesIntersectOwnerRoles(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "rita", "Reader")
	rita := e.As(t, "rita", pw)
	_, secret, err := e.Svc.CreateToken(rita, auth.TokenInput{
		Name: "greedy", Scopes: privs("system:*", "repo:*:admin"), ExpiresAt: e.Clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.Svc.Authenticate(context.Background(), core.Credentials{Token: secret})
	if err != nil {
		t.Fatal(err)
	}
	az := e.Svc.Authorizer()
	ctx := context.Background()
	if err := az.Authorize(ctx, p, core.ActionRead, core.RepoResource("npm-hosted")); err != nil {
		t.Fatalf("read within the role: %v", err)
	}
	for _, c := range []struct {
		a core.Action
		r core.Resource
	}{
		{core.ActionWrite, core.RepoResource("npm-hosted")},
		{core.ActionAdmin, core.RepoResource("npm-hosted")},
		{core.ActionAdmin, core.SystemResource(core.SystemUsers)},
		{core.ActionRead, core.SystemResource(core.SystemAudit)},
	} {
		if err := az.Authorize(ctx, p, c.a, c.r); !errors.Is(err, core.ErrForbidden) {
			t.Errorf("%s %s beyond the owner's roles: %v", c.a, c.r, err)
		}
	}
	got, err := az.Privileges(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if want := privs("repo:*:read"); !slices.Equal(got, want) {
		t.Fatalf("effective privileges = %v, want %v", got, want)
	}
}

func TestTokenRestrictions(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "pia", "Publisher")
	pia := e.As(t, "pia", pw)
	exp := e.Clock.Now().Add(24 * time.Hour)

	for _, bad := range [][]string{{"10.0.0.0/33"}, {"not-an-ip"}, {"fe80::1%eth0"}, make([]string, 33)} {
		if _, _, err := e.Svc.CreateToken(pia, auth.TokenInput{Name: "x", Scopes: privs("repo:*:read"), ExpiresAt: exp, AllowedCIDRs: bad}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("allowed_cidrs %v: err = %v, want ErrInvalid", bad, err)
		}
	}

	tok, secret, err := e.Svc.CreateToken(pia, auth.TokenInput{
		Name: "ci", Scopes: privs("repo:npm-*:write", "repo:npm-*:read"), ExpiresAt: exp,
		AllowedCIDRs: []string{"10.20.0.7/16", " 203.0.113.9 ", "10.20.1.1/16", "::ffff:192.0.2.0/120"}, ReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"10.20.0.0/16", "192.0.2.0/24", "203.0.113.9/32"}; !slices.Equal(tok.AllowedCIDRs, want) || !tok.ReadOnly {
		t.Fatalf("stored restrictions = %v read_only=%v, want %v", tok.AllowedCIDRs, tok.ReadOnly, want)
	}
	page, err := e.Svc.ListTokens(pia, auth.TokenQuery{})
	if err != nil || len(page.Items) != 1 || len(page.Items[0].AllowedCIDRs) != 3 || !page.Items[0].ReadOnly {
		t.Fatalf("listed = %+v, %v", page.Items, err)
	}

	for ip, ok := range map[string]bool{"10.20.99.1": true, "203.0.113.9": true, "::ffff:10.20.0.1": true, "192.0.2.77": true,
		"10.21.0.1": false, "203.0.113.10": false, "": false, "garbage": false} {
		p, err := e.Svc.Authenticate(from(ip), core.Credentials{Token: secret})
		if ok != (err == nil) {
			t.Errorf("from %q: err = %v, want allowed=%v", ip, err, ok)
			continue
		}
		if ok && (!p.ReadOnly || !slices.Equal(p.Scopes, privs("repo:npm-*:read"))) {
			t.Errorf("from %q: principal read_only=%v scopes=%v", ip, p.ReadOnly, p.Scopes)
		}
	}
	denied := 0
	for _, ev := range e.Audit.Events(auth.AuditTokenAuthFailed) {
		if ev.Details["reason"] == "address_not_allowed" {
			denied++
		}
	}
	if denied != 4 { // two outside addresses, an unknown and a malformed one
		t.Errorf("address_not_allowed audited %d times, want 4", denied)
	}

	ctx := from("10.20.0.1")
	p, err := e.Svc.Authenticate(ctx, core.Credentials{Token: secret})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.Authorizer().Authorize(ctx, p, core.ActionWrite, core.RepoResource("npm-hosted")); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("read-only token writes: %v", err)
	}
	// A read-only token cannot mint a token that may write.
	tokCtx := core.WithPrincipal(ctx, p)
	if _, _, err := e.Svc.CreateToken(tokCtx, auth.TokenInput{Name: "w", Scopes: privs("repo:npm-*:read"), ExpiresAt: exp}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("read-only token creates a writable token: %v", err)
	}

	// Session tokens minted from it keep both restrictions.
	sess, _, err := e.Svc.Issue(ctx, p, core.TokenRequest{Name: "registry", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(from("198.51.100.1"), core.Credentials{Token: sess}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("session token from outside the networks: %v", err)
	}
	sp, err := e.Svc.Authenticate(from("10.20.5.5"), core.Credentials{Token: sess})
	if err != nil || !sp.ReadOnly {
		t.Fatalf("session token: %+v, %v", sp, err)
	}
	cur, err := e.Svc.CurrentToken(core.WithPrincipal(ctx, sp))
	if err != nil || cur.Kind != "session" || !cur.ReadOnly || len(cur.AllowedCIDRs) != 3 || cur.ExpiresAt.After(exp) {
		t.Fatalf("current session token = %+v, %v", cur, err)
	}
	cur, err = e.Svc.CurrentToken(tokCtx)
	if err != nil || cur.ID != tok.ID || cur.Kind != "personal" || cur.Name != "ci" || !slices.Equal(cur.Scopes, privs("repo:npm-*:read")) {
		t.Fatalf("current token = %+v, %v", cur, err)
	}
	if cur, err := e.Svc.CurrentToken(pia); err != nil || cur != nil {
		t.Fatalf("current token of a password login = %+v, %v", cur, err)
	}
}

func TestRotateToken(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "olga", "Publisher")
	mustCreateUser(t, e, admin, "ivan", "Publisher")
	if _, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "deployer", Kind: auth.KindServiceAccount, Roles: []string{"Publisher"}}); err != nil {
		t.Fatal(err)
	}
	olga := e.As(t, "olga", pw)
	created := e.Clock.Now()
	old, oldSecret, err := e.Svc.CreateToken(olga, auth.TokenInput{
		Name: "laptop", Scopes: privs("repo:npm-*:write"), ExpiresAt: created.Add(30 * 24 * time.Hour),
		AllowedCIDRs: []string{"10.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.Clock.Advance(10 * 24 * time.Hour)

	// Others cannot see it; a person's token is only the person's to rotate.
	if _, err := e.Svc.RotateToken(e.As(t, "ivan", pw), old.ID, auth.RotateInput{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("ivan rotates olga's token: %v", err)
	}
	if _, err := e.Svc.RotateToken(admin, old.ID, auth.RotateInput{}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("admin rotates a person's token: %v", err)
	}
	if _, err := e.Svc.RotateToken(olga, "00000000-0000-4000-8000-000000000000", auth.RotateInput{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}

	// Keep the old token: both work.
	res, err := e.Svc.RotateToken(olga, old.ID, auth.RotateInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Token.Name != "laptop" || res.Token.Owner != "olga" || !slices.Equal(res.Token.Scopes, old.Scopes) ||
		!slices.Equal(res.Token.AllowedCIDRs, []string{"10.0.0.0/8"}) || res.Secret == oldSecret || res.RevokedNow {
		t.Fatalf("rotated = %+v", res)
	}
	if got := res.Token.ExpiresAt.Sub(e.Clock.Now()); got < 30*24*time.Hour-time.Second || got > 30*24*time.Hour {
		t.Fatalf("new lifetime = %s, want 30 days", got)
	}
	if !res.Old.ExpiresAt.Equal(old.ExpiresAt) {
		t.Fatalf("kept old token expiry changed: %s", res.Old.ExpiresAt)
	}
	for _, s := range []string{oldSecret, res.Secret} {
		if _, err := e.Svc.Authenticate(from("10.1.2.3"), core.Credentials{Token: s}); err != nil {
			t.Fatalf("token after rotation without revoke: %v", err)
		}
	}

	// Grace period: the old one ends a day from now.
	day := 24 * time.Hour
	res2, err := e.Svc.RotateToken(olga, old.ID, auth.RotateInput{RevokeOldAfter: &day})
	if err != nil {
		t.Fatal(err)
	}
	if want := e.Clock.Now().Add(day); !res2.Old.ExpiresAt.Equal(want.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("old token ends %s, want %s", res2.Old.ExpiresAt, want)
	}
	e.Clock.Advance(day + time.Minute)
	if _, err := e.Svc.Authenticate(from("10.1.2.3"), core.Credentials{Token: oldSecret}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("old token after the grace period: %v", err)
	}
	if _, err := e.Svc.Authenticate(from("10.1.2.3"), core.Credentials{Token: res2.Secret}); err != nil {
		t.Fatalf("replacement: %v", err)
	}

	// Revoke at once.
	zero := time.Duration(0)
	res3, err := e.Svc.RotateToken(olga, res2.Token.ID, auth.RotateInput{RevokeOldAfter: &zero})
	if err != nil || !res3.RevokedNow {
		t.Fatalf("rotate and revoke: %+v, %v", res3, err)
	}
	if _, err := e.Svc.Authenticate(from("10.1.2.3"), core.Credentials{Token: res2.Secret}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("revoked token still works: %v", err)
	}
	if ev := e.Audit.Events(auth.AuditTokenRotate); len(ev) < 3 || ev[len(ev)-1].Details["replaces"] != res2.Token.ID {
		t.Fatalf("rotate audit = %+v", ev)
	}

	// Service-account tokens: administrators rotate them.
	sa, _, err := e.Svc.CreateToken(admin, auth.TokenInput{Name: "deploy", Owner: "deployer", Scopes: privs("repo:*:write"), ExpiresAt: e.Clock.Now().Add(7 * day)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.RotateToken(olga, sa.ID, auth.RotateInput{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("non-admin rotates a service-account token: %v", err)
	}
	r4, err := e.Svc.RotateToken(admin, sa.ID, auth.RotateInput{RevokeOldAfter: &zero})
	if err != nil || r4.Token.Owner != "deployer" || r4.Token.OwnerKind != auth.KindServiceAccount {
		t.Fatalf("admin rotates SA token: %+v, %v", r4, err)
	}
	if _, err := e.Svc.RotateToken(admin, sa.ID, auth.RotateInput{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("rotating a revoked token: %v", err)
	}
}
