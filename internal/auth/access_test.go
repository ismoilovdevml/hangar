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

// login signs username in from ip and returns a context acting through the
// new browser session, plus the session.
func login(t *testing.T, e *authtest.Env, username, password, ip string) (context.Context, *auth.Session, string) {
	t.Helper()
	ctx := core.WithRequestInfo(context.Background(), core.RequestInfo{RemoteIP: ip, UserAgent: "test/" + ip})
	p, err := e.Svc.Login(ctx, username, password)
	if err != nil {
		t.Fatalf("login %s: %v", username, err)
	}
	secret, sess, err := e.Svc.CreateSession(ctx, p, false)
	if err != nil {
		t.Fatal(err)
	}
	return core.WithPrincipal(ctx, p), sess, secret
}

func TestUserFilters(t *testing.T) {
	e := authtest.New(t, auth.Config{Lockout: auth.Lockout{MaxAttempts: 1, Window: time.Minute, Duration: time.Hour}})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "ann", "Reader")
	bob := mustCreateUser(t, e, admin, "bob", "Publisher")
	mustCreateUser(t, e, admin, "cat", "Reader")
	if _, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "ci", Kind: auth.KindServiceAccount, Description: ptr("GitLab runners"), Roles: []string{"CI deployer"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.SetUserDisabled(admin, bob.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Login(context.Background(), "cat", "wrong password!!"); err == nil {
		t.Fatal("wrong password accepted")
	}
	names := func(q auth.UserQuery) []string {
		t.Helper()
		pg, err := e.Svc.ListUsers(admin, q)
		if err != nil {
			t.Fatalf("list %+v: %v", q, err)
		}
		var out []string
		for _, u := range pg.Items {
			out = append(out, u.Username)
		}
		return out
	}
	for _, tc := range []struct {
		q    auth.UserQuery
		want []string
	}{
		{auth.UserQuery{Role: "Reader"}, []string{"ann", "cat"}},
		{auth.UserQuery{Status: auth.StatusDisabled}, []string{"bob"}},
		{auth.UserQuery{Status: auth.StatusLocked}, []string{"cat"}},
		{auth.UserQuery{Status: auth.StatusActive, Kind: auth.KindUser}, []string{"admin", "ann"}},
		{auth.UserQuery{Kind: auth.KindServiceAccount}, []string{"ci"}},
		{auth.UserQuery{Source: "ldap"}, nil},
		{auth.UserQuery{Role: "Reader", Q: "an"}, []string{"ann"}},
	} {
		if got := names(tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("%+v: got %v, want %v", tc.q, got, tc.want)
		}
	}
	for _, q := range []auth.UserQuery{{Status: "gone"}, {Source: "kerberos"}, {Kind: "robot"}} {
		if _, err := e.Svc.ListUsers(admin, q); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%+v: err = %v, want invalid", q, err)
		}
	}
	ci, err := e.Svc.GetUserByName(context.Background(), "ci")
	if err != nil || ci.Description != "GitLab runners" || ci.PasswordSet {
		t.Fatalf("service account = %+v, %v", ci, err)
	}
	if u, _ := e.Svc.GetUserByName(context.Background(), "ann"); !u.PasswordSet {
		t.Fatal("ann should have a password")
	}
}

func TestDisableEnableUnlock(t *testing.T) {
	e := authtest.New(t, auth.Config{Lockout: auth.Lockout{MaxAttempts: 1, Window: time.Minute, Duration: time.Hour}})
	admin := e.Admin(t)
	adminUser := mustUser(t, e, "admin")
	dan := mustCreateUser(t, e, admin, "dan", "Reader")
	danCtx, _, secret := login(t, e, "dan", pw, "192.0.2.1")
	tok, tokSecret, err := e.Svc.CreateToken(danCtx, auth.TokenInput{Name: "cli", Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:read")}, ExpiresAt: e.Clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	// Guards: not yourself, not the last administrator, only with system:users:write.
	if _, err := e.Svc.SetUserDisabled(admin, adminUser.ID, true); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("disable self: %v", err)
	}
	if _, err := e.Svc.SetUserDisabled(danCtx, adminUser.ID, true); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("reader disables admin: %v", err)
	}
	other := mustCreateUser(t, e, admin, "eve", auth.AdminRole)
	if _, err := e.Svc.SetUserDisabled(core.WithPrincipal(context.Background(), principalOf(t, e, "eve")), adminUser.ID, true); err != nil {
		t.Fatalf("second admin disables first: %v", err)
	}
	if _, err := e.Svc.SetUserDisabled(admin, other.ID, true); err == nil {
		t.Fatal("disabled admin acted")
	}
	if _, err := e.Svc.SetUserDisabled(core.WithPrincipal(context.Background(), principalOf(t, e, "eve")), adminUser.ID, false); err != nil {
		t.Fatal(err)
	}
	admin = e.As(t, "admin", authtest.AdminPassword)
	if _, err := e.Svc.SetUserDisabled(admin, other.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.SetUserDisabled(core.WithPrincipal(context.Background(), principalOf(t, e, "admin")), adminUser.ID, true); err == nil {
		t.Fatal("last enabled admin disabled")
	}

	// Disabling ends sessions and stops tokens; enabling restores tokens only.
	u, err := e.Svc.SetUserDisabled(admin, dan.ID, true)
	if err != nil || !u.Disabled {
		t.Fatalf("disable = %+v, %v", u, err)
	}
	if _, _, err := e.Svc.SessionPrincipal(context.Background(), secret); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("session after disable: %v", err)
	}
	if _, err := e.Svc.Authenticate(context.Background(), core.Credentials{Token: tokSecret}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("token after disable: %v", err)
	}
	if _, err := e.Svc.SetUserDisabled(admin, dan.ID, false); err != nil {
		t.Fatal(err)
	}
	if p, err := e.Svc.Authenticate(context.Background(), core.Credentials{Token: tokSecret}); err != nil || p.TokenID != tok.ID {
		t.Fatalf("token after enable: %v", err)
	}
	if n := len(e.Audit.Events(auth.AuditUserDisable)); n < 2 {
		t.Errorf("disable audited %d times", n)
	}
	if n := len(e.Audit.Events(auth.AuditUserEnable)); n < 2 {
		t.Errorf("enable audited %d times", n)
	}

	// Unlock.
	if _, err := e.Svc.Login(context.Background(), "dan", "wrong password!!"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, err := e.Svc.Login(context.Background(), "dan", pw); !isLocked(err) {
		t.Fatalf("expected lock, got %v", err)
	}
	if _, err := e.Svc.UnlockUser(danCtx, dan.ID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("reader unlocks: %v", err)
	}
	u, err = e.Svc.UnlockUser(admin, dan.ID)
	if err != nil || u.LockedUntil != nil {
		t.Fatalf("unlock = %+v, %v", u, err)
	}
	if _, err := e.Svc.Login(context.Background(), "dan", pw); err != nil {
		t.Fatalf("login after unlock: %v", err)
	}
}

// succeeded counts successful audit events with action.
func succeeded(e *authtest.Env, action string) int {
	n := 0
	for _, ev := range e.Audit.Events(action) {
		if ev.Outcome == core.OutcomeSuccess {
			n++
		}
	}
	return n
}

func principalOf(t *testing.T, e *authtest.Env, name string) *core.Principal {
	t.Helper()
	pw := pw
	if name == "admin" {
		pw = authtest.AdminPassword
	}
	p, err := e.Svc.Authenticate(context.Background(), core.Credentials{Username: name, Password: pw})
	if err != nil {
		t.Fatalf("authenticate %s: %v", name, err)
	}
	return p
}

func TestPasswordChangeAndReset(t *testing.T) {
	e := authtest.New(t, auth.Config{Lockout: auth.Lockout{MaxAttempts: 3, Window: time.Minute, Duration: time.Hour}})
	admin := e.Admin(t)
	fay := mustCreateUser(t, e, admin, "fay", "Reader")
	ctx1, s1, secret1 := login(t, e, "fay", pw, "192.0.2.1")
	_, _, secret2 := login(t, e, "fay", pw, "192.0.2.2")
	pat, patSecret, err := e.Svc.CreateToken(ctx1, auth.TokenInput{Name: "laptop", Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:read")}, ExpiresAt: e.Clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	reg, _, err := e.Svc.Issue(ctx1, core.PrincipalFrom(ctx1), core.TokenRequest{Name: "registry"})
	if err != nil {
		t.Fatal(err)
	}

	const next = "an entirely new passphrase"
	if err := e.Svc.ChangePassword(ctx1, "not my password", next, s1.ID); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("wrong current: %v", err)
	}
	if err := e.Svc.ChangePassword(ctx1, pw, "short", s1.ID); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("weak new: %v", err)
	}
	if err := e.Svc.ChangePassword(ctx1, pw, pw, s1.ID); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("same password: %v", err)
	}
	// A token cannot change the password.
	tp, err := e.Svc.Authenticate(context.Background(), core.Credentials{Token: patSecret})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.ChangePassword(core.WithPrincipal(context.Background(), tp), pw, next, ""); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("token changes password: %v", err)
	}
	if err := e.Svc.ChangePassword(context.Background(), pw, next, ""); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("anonymous changes password: %v", err)
	}
	if err := e.Svc.ChangePassword(ctx1, pw, next, s1.ID); err != nil {
		t.Fatal(err)
	}
	// The current session survives, the other one and session tokens end,
	// personal access tokens keep working.
	if _, _, err := e.Svc.SessionPrincipal(context.Background(), secret1); err != nil {
		t.Fatalf("current session ended: %v", err)
	}
	if _, _, err := e.Svc.SessionPrincipal(context.Background(), secret2); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("other session survived: %v", err)
	}
	if _, err := e.Svc.Authenticate(context.Background(), core.Credentials{Token: reg}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("session token survived: %v", err)
	}
	if p, err := e.Svc.Authenticate(context.Background(), core.Credentials{Token: patSecret}); err != nil || p.TokenID != pat.ID {
		t.Fatalf("personal token stopped: %v", err)
	}
	if _, err := e.Svc.Login(context.Background(), "fay", next); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
	ev := e.Audit.Events(auth.AuditUserPassword)
	if len(ev) == 0 || ev[len(ev)-1].Outcome != core.OutcomeSuccess || ev[len(ev)-1].Details["self"] != true {
		t.Fatalf("password change audit = %+v", ev)
	}

	// Wrong current passwords count towards the lockout.
	ctx3, s3, _ := login(t, e, "fay", next, "192.0.2.3")
	_ = e.Svc.ChangePassword(ctx3, "guess one!!!!", "whatever passphrase", s3.ID)
	_ = e.Svc.ChangePassword(ctx3, "guess two!!!!", "whatever passphrase", s3.ID)
	if err := e.Svc.ChangePassword(ctx3, "guess three!!", "whatever passphrase", s3.ID); !isLocked(err) {
		t.Fatalf("third wrong guess: %v", err)
	}

	// Admin reset: not for yourself, ends every session, clears the lock.
	if err := e.Svc.ResetPassword(admin, mustUser(t, e, "admin").ID, "another admin passphrase"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("admin resets own password: %v", err)
	}
	if err := e.Svc.ResetPassword(ctx1, fay.ID, "reset by herself!!"); !errors.Is(err, core.ErrInvalid) && !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("reader resets: %v", err)
	}
	if err := e.Svc.ResetPassword(admin, fay.ID, "x"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("weak reset: %v", err)
	}
	if err := e.Svc.ResetPassword(admin, fay.ID, "reset by the admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Svc.SessionPrincipal(context.Background(), secret1); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("session survived reset: %v", err)
	}
	if _, err := e.Svc.Login(context.Background(), "fay", "reset by the admin"); err != nil {
		t.Fatalf("login after reset: %v", err)
	}
	sa, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "bot", Kind: auth.KindServiceAccount})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.ResetPassword(admin, sa.ID, "service accounts have none"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("reset service account: %v", err)
	}
	// UpdateUser no longer sets the caller's own password.
	if _, err := e.Svc.UpdateUser(admin, mustUser(t, e, "admin").ID, auth.UserInput{Username: "admin", Kind: auth.KindUser, Password: ptr("yet another passphrase")}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("own password via update: %v", err)
	}
}

func TestUpdateProfile(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "gus", "Reader")
	mustCreateUser(t, e, admin, "hy")
	gus := e.As(t, "gus", pw)
	u, err := e.Svc.UpdateProfile(gus, auth.ProfileInput{DisplayName: ptr("Gus G."), Email: ptr("gus@corp.example")})
	if err != nil || u.DisplayName != "Gus G." || u.Email != "gus@corp.example" || slices.Compare(u.Roles, []string{"Reader"}) != 0 {
		t.Fatalf("profile = %+v, %v", u, err)
	}
	if _, err := e.Svc.UpdateProfile(gus, auth.ProfileInput{Email: ptr("not an email")}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad email: %v", err)
	}
	if _, err := e.Svc.UpdateProfile(gus, auth.ProfileInput{Email: ptr("HY@example.test")}); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("taken email: %v", err)
	}
	if _, err := e.Svc.UpdateProfile(context.Background(), auth.ProfileInput{}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("anonymous: %v", err)
	}
	_, secret, err := e.Svc.CreateToken(gus, auth.TokenInput{Name: "t", Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:read")}, ExpiresAt: e.Clock.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	tp, _ := e.Svc.Authenticate(context.Background(), core.Credentials{Token: secret})
	if _, err := e.Svc.UpdateProfile(core.WithPrincipal(context.Background(), tp), auth.ProfileInput{DisplayName: ptr("x")}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("scoped token: %v", err)
	}
	if ev := e.Audit.Events(auth.AuditUserUpdate); len(ev) != 1 || ev[0].Details["self"] != true {
		t.Fatalf("audit = %+v", ev)
	}
}

func TestSessionManagement(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	hal := mustCreateUser(t, e, admin, "hal", "Reader")
	mustCreateUser(t, e, admin, "ida", "Reader")
	h1, s1, secret1 := login(t, e, "hal", pw, "192.0.2.1")
	_, s2, secret2 := login(t, e, "hal", pw, "2001:db8::2")
	_, s3, secret3 := login(t, e, "hal", pw, "192.0.2.3")

	list, err := e.Svc.ListSessions(h1, hal.ID, s1.ID)
	if err != nil || len(list) != 3 {
		t.Fatalf("sessions = %+v, %v", list, err)
	}
	var current int
	for _, si := range list {
		if si.Current {
			current++
			if si.ID != s1.ID || si.IP == nil || *si.IP != "192.0.2.1" || si.UserAgent != "test/192.0.2.1" {
				t.Fatalf("current session = %+v", si)
			}
		}
	}
	if current != 1 {
		t.Fatalf("%d current sessions", current)
	}
	// Other users see nothing of hal's sessions without system:users.
	ida := e.As(t, "ida", pw)
	if _, err := e.Svc.ListSessions(ida, hal.ID, ""); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("other user lists: %v", err)
	}
	if err := e.Svc.RevokeSession(ida, hal.ID, s2.ID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("other user revokes: %v", err)
	}
	if _, err := e.Svc.ListSessions(context.Background(), hal.ID, ""); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("anonymous lists: %v", err)
	}
	// Revoke one: its id belongs to hal, so ida's id path cannot reach it.
	if err := e.Svc.RevokeSession(ida, mustUser(t, e, "ida").ID, s2.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("revoke someone else's session as own: %v", err)
	}
	if err := e.Svc.RevokeSession(h1, hal.ID, s2.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Svc.SessionPrincipal(context.Background(), secret2); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatal("revoked session still works")
	}
	if err := e.Svc.RevokeSession(h1, hal.ID, s2.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("revoke twice: %v", err)
	}
	// End all others keeps the current one.
	n, err := e.Svc.RevokeSessions(h1, hal.ID, s1.ID)
	if err != nil || n != 1 {
		t.Fatalf("revoke others = %d, %v", n, err)
	}
	if _, _, err := e.Svc.SessionPrincipal(context.Background(), secret3); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatal("other session survived")
	}
	if _, _, err := e.Svc.SessionPrincipal(context.Background(), secret1); err != nil {
		t.Fatalf("current session ended: %v", err)
	}
	_ = s3
	// An administrator ends everything.
	if n, err := e.Svc.RevokeSessions(admin, hal.ID, ""); err != nil || n != 1 {
		t.Fatalf("admin revoke = %d, %v", n, err)
	}
	if _, _, err := e.Svc.SessionPrincipal(context.Background(), secret1); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatal("session survived admin revoke")
	}
	if got := succeeded(e, auth.AuditSessionRevoke); got != 1 {
		t.Errorf("session.revoke audited %d times", got)
	}
	if got := succeeded(e, auth.AuditSessionsRevoke); got != 2 {
		t.Errorf("session.revoke_all audited %d times", got)
	}
}

func TestTokenListingAndWarnings(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "jo", "Publisher")
	mustCreateUser(t, e, admin, "kim", "Reader")
	if _, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "deploy-bot", Kind: auth.KindServiceAccount, Roles: []string{"CI deployer"}}); err != nil {
		t.Fatal(err)
	}
	jo := core.WithRequestInfo(e.As(t, "jo", pw), core.RequestInfo{RemoteIP: "::ffff:198.51.100.9"})
	kim := e.As(t, "kim", pw)
	now := e.Clock.Now()
	read := []core.Privilege{core.MustParsePrivilege("repo:*:read")}
	mk := func(ctx context.Context, in auth.TokenInput) *auth.TokenInfo {
		t.Helper()
		tok, _, err := e.Svc.CreateToken(ctx, in)
		if err != nil {
			t.Fatalf("create %s: %v", in.Name, err)
		}
		return tok
	}
	soon := mk(jo, auth.TokenInput{Name: "laptop soon", Scopes: read, ExpiresAt: now.Add(3 * 24 * time.Hour)})
	later := mk(jo, auth.TokenInput{Name: "ci later", Scopes: []core.Privilege{core.MustParsePrivilege("repo:npm-*:write"), core.MustParsePrivilege("repo:npm-*:write"), core.MustParsePrivilege("repo:*:read")}, ExpiresAt: now.Add(90 * 24 * time.Hour)})
	if len(later.Scopes) != 2 || later.OwnerKind != auth.KindUser {
		t.Fatalf("scopes not de-duplicated: %+v", later)
	}
	kimTok := mk(kim, auth.TokenInput{Name: "kim", Scopes: read, ExpiresAt: now.Add(2 * time.Hour)})
	saTok := mk(admin, auth.TokenInput{Name: "deploy", Owner: "deploy-bot", Scopes: read, ExpiresAt: now.Add(5 * 24 * time.Hour)})
	if saTok.OwnerKind != auth.KindServiceAccount {
		t.Fatalf("sa token owner kind = %q", saTok.OwnerKind)
	}

	// Scope validation.
	for name, scopes := range map[string][]core.Privilege{
		"unknown area": {core.MustParsePrivilege("system:nuclear:read")},
		"repo any":     {core.MustParsePrivilege("repo:*:*")},
		"bad pattern":  {core.MustParsePrivilege("repo:NPM/x:read")},
	} {
		if _, _, err := e.Svc.CreateToken(jo, auth.TokenInput{Name: name, Scopes: scopes, ExpiresAt: now.Add(time.Hour)}); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	// Last use is recorded with the client address.
	_, secret, err := e.Svc.CreateToken(jo, auth.TokenInput{Name: "used", Scopes: read, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(core.WithRequestInfo(context.Background(), core.RequestInfo{RemoteIP: "203.0.113.5"}), core.Credentials{Token: secret}); err != nil {
		t.Fatal(err)
	}
	e.Svc.FlushUsage(context.Background())
	pg, err := e.Svc.ListTokens(jo, auth.TokenQuery{Q: "used"})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].LastUsedAt == nil || pg.Items[0].LastUsedIP == nil || *pg.Items[0].LastUsedIP != "203.0.113.5" {
		t.Fatalf("used token = %+v, %v", pg.Items, err)
	}

	// The admin listing.
	if _, err := e.Svc.ListTokens(kim, auth.TokenQuery{All: true}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("reader lists all: %v", err)
	}
	all, err := e.Svc.ListTokens(admin, auth.TokenQuery{All: true})
	if err != nil || len(all.Items) != 5 {
		t.Fatalf("all tokens = %d, %v", len(all.Items), err)
	}
	sas, err := e.Svc.ListTokens(admin, auth.TokenQuery{All: true, OwnerKind: auth.KindServiceAccount})
	if err != nil || len(sas.Items) != 1 || sas.Items[0].ID != saTok.ID {
		t.Fatalf("service account tokens = %+v, %v", sas.Items, err)
	}
	if one, err := e.Svc.ListTokens(admin, auth.TokenQuery{All: true, Owner: "kim"}); err != nil || len(one.Items) != 1 || one.Items[0].ID != kimTok.ID {
		t.Fatalf("all narrowed to kim = %+v, %v", one.Items, err)
	}
	e.Clock.Advance(3 * time.Hour)
	if exp, err := e.Svc.ListTokens(admin, auth.TokenQuery{All: true, Status: auth.TokenExpired}); err != nil || len(exp.Items) != 2 {
		t.Fatalf("expired = %d, %v", len(exp.Items), err)
	}
	if act, err := e.Svc.ListTokens(jo, auth.TokenQuery{Status: auth.TokenActive}); err != nil || len(act.Items) != 2 {
		t.Fatalf("jo active = %d, %v", len(act.Items), err)
	}
	if _, err := e.Svc.ListTokens(jo, auth.TokenQuery{Status: "zombie"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad status: %v", err)
	}

	// Warnings: jo sees her token that expires in < 14 days; the admin sees
	// the service account's; kim's token already expired.
	w, err := e.Svc.TokenWarnings(jo)
	if err != nil || len(w) != 1 || w[0].ID != soon.ID {
		t.Fatalf("jo warnings = %+v, %v", w, err)
	}
	w, err = e.Svc.TokenWarnings(admin)
	if err != nil || len(w) != 1 || w[0].ID != saTok.ID {
		t.Fatalf("admin warnings = %+v, %v", w, err)
	}
	if w, err := e.Svc.TokenWarnings(kim); err != nil || len(w) != 0 {
		t.Fatalf("kim warnings = %+v, %v", w, err)
	}
	if w, err := e.Svc.TokenWarnings(context.Background()); err != nil || len(w) != 0 {
		t.Fatalf("anonymous warnings = %+v, %v", w, err)
	}
}

func TestRoleBindingsAndValidation(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	adminUser := mustUser(t, e, "admin")
	lu := mustCreateUser(t, e, admin, "lu")
	luCtx := e.As(t, "lu", pw)
	if err := e.Svc.BindRole(luCtx, "Reader", lu.ID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("self bind: %v", err)
	}
	if err := e.Svc.BindRole(admin, "Reader", lu.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.BindRole(admin, "Reader", lu.ID); err != nil {
		t.Fatalf("bind twice: %v", err)
	}
	if err := e.Svc.BindRole(admin, "Nope", lu.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown role: %v", err)
	}
	if err := e.Svc.BindRole(admin, "Reader", "00000000-0000-0000-0000-000000000000"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	if u := mustUser(t, e, "lu"); !slices.Equal(u.Roles, []string{"Reader"}) {
		t.Fatalf("roles = %v", u.Roles)
	}
	if err := e.Svc.UnbindRole(admin, auth.AdminRole, adminUser.ID); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("unbind last admin: %v", err)
	}
	if err := e.Svc.UnbindRole(admin, "Reader", lu.ID); err != nil {
		t.Fatal(err)
	}
	if got := succeeded(e, auth.AuditRoleBind); got != 1 {
		t.Errorf("bind audited %d times", got)
	}
	if got := succeeded(e, auth.AuditRoleUnbind); got != 1 {
		t.Errorf("unbind audited %d times", got)
	}
	for _, privs := range [][]string{{"system:nuclear:read"}, {"repo:Docker:read"}, {"repo:*:*"}} {
		in := auth.RoleInput{Name: "bad", Privileges: []core.Privilege{core.MustParsePrivilege(privs[0])}}
		if _, err := e.Svc.CreateRole(admin, in); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%v: %v", privs, err)
		}
	}
	if _, err := e.Svc.CreateRole(admin, auth.RoleInput{Name: "ok", Privileges: []core.Privilege{
		core.MustParsePrivilege("repo:npm-[ab]*:write"), core.MustParsePrivilege("system:*"), core.MustParsePrivilege("system:audit:read"),
	}}); err != nil {
		t.Fatal(err)
	}
}

func TestEffectivePermissions(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	for _, r := range []struct {
		name string
		anon bool
	}{{"npm-internal", false}, {"npm-public", true}, {"docker-prod", false}} {
		repo := &core.Repository{Name: r.name, Format: "raw", Type: core.RepoHosted, BlobStore: "default", Online: true}
		repo.Config.AnonymousRead = r.anon
		if _, err := e.Repos.Create(context.Background(), repo); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Svc.CreateRole(admin, auth.RoleInput{Name: "npm team", Privileges: []core.Privilege{
		core.MustParsePrivilege("repo:npm-*:write"), core.MustParsePrivilege("system:audit:read"),
	}}); err != nil {
		t.Fatal(err)
	}
	mo := mustCreateUser(t, e, admin, "mo", "npm team", "Reader")
	mustCreateUser(t, e, admin, "nat")

	ep, err := e.Svc.EffectivePermissions(admin, mo.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]auth.ResourcePermissions{}
	for _, r := range ep.Repositories {
		byName[r.Name] = r
	}
	if got := byName["npm-internal"].Actions; !slices.Equal(got, []core.Action{core.ActionRead, core.ActionWrite}) {
		t.Fatalf("npm-internal actions = %v", got)
	}
	if got := byName["docker-prod"].Actions; !slices.Equal(got, []core.Action{core.ActionRead}) {
		t.Fatalf("docker-prod actions = %v", got)
	}
	pub := byName["npm-public"]
	if !pub.AnonymousRead || pub.Format != "raw" || pub.Type != "hosted" {
		t.Fatalf("npm-public = %+v", pub)
	}
	var sources []string
	for _, g := range pub.Grants {
		if g.Action == core.ActionRead {
			sources = append(sources, g.Source+":"+g.Role)
		}
	}
	if !slices.Equal(sources, []string{"role:Reader", "anonymous:"}) {
		t.Fatalf("npm-public read grants = %v", sources)
	}
	for _, g := range byName["npm-internal"].Grants {
		if g.Action == core.ActionWrite && (g.Role != "npm team" || g.Privilege == nil || g.Privilege.String() != "repo:npm-*:write") {
			t.Fatalf("write grant = %+v", g)
		}
	}
	if len(ep.System) != 1 || ep.System[0].Name != core.SystemAudit || !slices.Equal(ep.System[0].Actions, []core.Action{core.ActionRead}) {
		t.Fatalf("system = %+v", ep.System)
	}
	if len(ep.Roles) != 2 || ep.Disabled || ep.Locked {
		t.Fatalf("roles/state = %+v", ep)
	}

	// A user without roles reaches only anonymous repositories.
	nat := e.As(t, "nat", pw)
	self, err := e.Svc.EffectivePermissions(nat, mustUser(t, e, "nat").ID)
	if err != nil || len(self.Repositories) != 1 || self.Repositories[0].Name != "npm-public" || len(self.System) != 0 {
		t.Fatalf("nat = %+v, %v", self, err)
	}
	// Others' permissions need system:users:read.
	if _, err := e.Svc.EffectivePermissions(nat, mo.ID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("nat reads mo: %v", err)
	}
	if _, err := e.Svc.EffectivePermissions(context.Background(), mo.ID); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("anonymous reads mo: %v", err)
	}
	// The admin has everything everywhere.
	adm, err := e.Svc.EffectivePermissions(admin, mustUser(t, e, "admin").ID)
	if err != nil || len(adm.Repositories) != 3 || len(adm.System) != len(auth.SystemAreas) {
		t.Fatalf("admin = %d repos, %d areas, %v", len(adm.Repositories), len(adm.System), err)
	}
	for _, r := range adm.Repositories {
		if len(r.Actions) != 4 {
			t.Fatalf("admin on %s = %v", r.Name, r.Actions)
		}
	}
}
