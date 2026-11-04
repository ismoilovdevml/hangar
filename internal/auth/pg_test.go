package auth_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

const pw = "a long enough passphrase"

func ptr[T any](v T) *T { return &v }

func mustCreateUser(t *testing.T, e *authtest.Env, admin context.Context, name string, roles ...string) *auth.User {
	t.Helper()
	in := auth.UserInput{Username: name, Kind: auth.KindUser, Password: ptr(pw), Roles: roles, Email: ptr(name + "@example.test")}
	u, err := e.Svc.CreateUser(admin, in)
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	return u
}

func TestMigrationUpDown(t *testing.T) {
	pool := authtest.Pool(t, false)
	ctx := context.Background()
	p := authtest.Provider(t, pool)
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(ctx, 6); err != nil {
		t.Fatalf("down to 6: %v", err)
	}
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('sessions') IS NULL`).Scan(&ok); err != nil || !ok {
		t.Fatalf("sessions table left after down (err=%v)", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("second up: %v", err)
	}
}

func TestBootstrapAdmin(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	ctx := context.Background()
	if _, err := e.Svc.EnsureBootstrapAdmin(ctx, "admin", "short", false); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("weak password: err = %v", err)
	}
	created, err := e.Svc.EnsureBootstrapAdmin(ctx, "admin", authtest.AdminPassword, false)
	if err != nil || !created {
		t.Fatalf("first bootstrap = %v, %v", created, err)
	}
	created, err = e.Svc.EnsureBootstrapAdmin(ctx, "admin", "another long passphrase", false)
	if err != nil || created {
		t.Fatalf("second bootstrap = %v, %v; want no-op", created, err)
	}
	p, err := e.Svc.Login(ctx, "admin", authtest.AdminPassword)
	if err != nil {
		t.Fatalf("login with bootstrap password: %v", err)
	}
	if !slices.Contains(p.Roles, auth.AdminRole) {
		t.Fatalf("roles = %v", p.Roles)
	}
	if err := e.Svc.Authorizer().Authorize(ctx, p, core.ActionAdmin, core.SystemResource(core.SystemSettings)); err != nil {
		t.Fatalf("admin not allowed: %v", err)
	}
	if ev := e.Audit.Events(auth.AuditUserBootstrap); len(ev) != 1 || ev[0].Actor != auth.SystemActor {
		t.Fatalf("bootstrap audit = %+v", ev)
	}
}

func TestBootstrapPromotesExistingWhenNoAdmin(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	u := mustCreateUser(t, e, admin, "ops")
	// Remove the only admin directly, as a broken install would look.
	if _, err := e.Pool.Exec(context.Background(), `DELETE FROM users WHERE username = 'admin'`); err != nil {
		t.Fatal(err)
	}
	created, err := e.Svc.EnsureBootstrapAdmin(context.Background(), "ops", "recovered passphrase!", false)
	if err != nil || !created {
		t.Fatalf("promote = %v, %v", created, err)
	}
	p, err := e.Svc.Login(context.Background(), "ops", "recovered passphrase!")
	if err != nil || p.ID != u.ID || !slices.Contains(p.Roles, auth.AdminRole) {
		t.Fatalf("login after promote: %+v, %v", p, err)
	}
}

func TestLoginLockout(t *testing.T) {
	e := authtest.New(t, auth.Config{Lockout: auth.Lockout{MaxAttempts: 3, Window: 10 * time.Minute, Duration: 15 * time.Minute}})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "alice", "Reader")
	ctx := context.Background()

	// Two failures, then the window passes: the count starts over.
	for range 2 {
		if _, err := e.Svc.Login(ctx, "alice", "wrong password!!"); !errors.Is(err, core.ErrUnauthenticated) {
			t.Fatalf("wrong password: %v", err)
		}
	}
	e.Clock.Advance(11 * time.Minute)
	for range 2 {
		if _, err := e.Svc.Login(ctx, "alice", "wrong password!!"); err == nil || isLocked(err) {
			t.Fatalf("locked too early: %v", err)
		}
	}
	// Third failure inside the window locks.
	_, err := e.Svc.Login(ctx, "alice", "wrong password!!")
	var locked *auth.LockedError
	if !errors.As(err, &locked) || !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("third failure: %v, want LockedError", err)
	}
	if d := locked.Until.Sub(e.Clock.Now()); d != 15*time.Minute {
		t.Fatalf("locked for %v", d)
	}
	// The correct password does not help while locked, for login or Basic.
	if _, err := e.Svc.Login(ctx, "alice", pw); !isLocked(err) {
		t.Fatalf("correct password while locked: %v", err)
	}
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Username: "alice", Password: pw}); !isLocked(err) {
		t.Fatalf("basic while locked: %v", err)
	}
	e.Clock.Advance(16 * time.Minute)
	if _, err := e.Svc.Login(ctx, "alice", pw); err != nil {
		t.Fatalf("after lock expired: %v", err)
	}
	var failed int
	if err := e.Pool.QueryRow(ctx, `SELECT failed_logins FROM users WHERE username = 'alice'`).Scan(&failed); err != nil || failed != 0 {
		t.Fatalf("failed_logins = %d (err=%v), want reset", failed, err)
	}

	// Every attempt is audited.
	fails := e.Audit.Events(auth.AuditLoginFailed)
	if len(fails) != 7 {
		t.Fatalf("failed attempts audited = %d, want 7", len(fails))
	}
	reasons := map[string]int{}
	for _, f := range fails {
		reasons[f.Details["reason"].(string)]++
		if f.Outcome != core.OutcomeDenied || f.Actor != "alice" {
			t.Errorf("bad audit event %+v", f)
		}
	}
	if reasons["bad_password"] != 5 || reasons["locked"] != 2 {
		t.Fatalf("reasons = %v", reasons)
	}
	if n := len(e.Audit.Events(auth.AuditLogin)); n != 1 {
		t.Fatalf("successful logins audited = %d, want 1", n)
	}
}

func isLocked(err error) bool {
	var l *auth.LockedError
	return errors.As(err, &l)
}

func TestLoginRejections(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "bob")
	sa, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "ci-bot", Kind: auth.KindServiceAccount})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := e.Svc.UpdateUser(admin, mustUser(t, e, "bob").ID, auth.UserInput{Username: "bob", Kind: auth.KindUser, Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string][2]string{
		"unknown":  {"nobody", pw},
		"disabled": {"bob", pw},
		"service":  {sa.Username, pw},
		"empty":    {"", pw},
	} {
		if _, err := e.Svc.Login(ctx, c[0], c[1]); !errors.Is(err, core.ErrUnauthenticated) || isLocked(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	reasons := []string{}
	for _, ev := range e.Audit.Events(auth.AuditLoginFailed) {
		reasons = append(reasons, ev.Details["reason"].(string))
	}
	slices.Sort(reasons)
	if want := []string{"disabled", "malformed", "no_local_password", "unknown_user"}; !slices.Equal(reasons, want) {
		t.Fatalf("reasons = %v, want %v", reasons, want)
	}
}

func mustUser(t *testing.T, e *authtest.Env, name string) *auth.User {
	t.Helper()
	u, err := e.Svc.GetUserByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestRehashOnParameterChange(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "carol")
	stronger := authtest.FastArgon2
	stronger.Memory, stronger.Iterations = 2048, 2
	e2 := authtest.NewOnPool(t, e.Pool, auth.Config{Argon2: stronger}, e.Clock)
	if _, err := e2.Svc.Login(context.Background(), "carol", pw); err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := e.Pool.QueryRow(context.Background(), `SELECT password_hash FROM users WHERE username = 'carol'`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hash, "m=2048,t=2,p=1") {
		t.Fatalf("hash not upgraded: %s", hash)
	}
	if _, err := e2.Svc.Login(context.Background(), "carol", pw); err != nil {
		t.Fatalf("login after rehash: %v", err)
	}
}

func TestBasicCacheDroppedOnPasswordChange(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	u := mustCreateUser(t, e, admin, "dave", "Reader")
	ctx := context.Background()
	creds := core.Credentials{Username: "dave", Password: pw}
	for range 3 {
		p, err := e.Svc.Authenticate(ctx, creds)
		if err != nil || p.Name != "dave" || p.Source != auth.SourceLocal || !slices.Equal(p.Roles, []string{"Reader"}) {
			t.Fatalf("basic = %+v, %v", p, err)
		}
	}
	if _, err := e.Svc.UpdateUser(admin, u.ID, auth.UserInput{Username: "dave", Kind: auth.KindUser, Password: ptr("a brand new passphrase")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(ctx, creds); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("old password still accepted: %v", err)
	}
	if p, err := e.Svc.Authenticate(ctx, core.Credentials{Username: "dave", Password: "a brand new passphrase"}); err != nil || p.Name != "dave" {
		t.Fatalf("new password: %v", err)
	}
	if p, _ := e.Svc.Authenticate(ctx, core.Credentials{}); !p.IsAnonymous() {
		t.Fatal("empty credentials must be anonymous")
	}
}

func TestPersonalTokens(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "erin", "Publisher")
	mustCreateUser(t, e, admin, "frank", "Reader")
	erin := e.As(t, "erin", pw)
	ctx := context.Background()
	exp := e.Clock.Now().Add(24 * time.Hour)

	for name, in := range map[string]auth.TokenInput{
		"no name":    {Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:read")}, ExpiresAt: exp},
		"no scopes":  {Name: "ci", ExpiresAt: exp},
		"no expiry":  {Name: "ci", Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:read")}},
		"past":       {Name: "ci", Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:read")}, ExpiresAt: e.Clock.Now().Add(-time.Minute)},
		"too long":   {Name: "ci", Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:read")}, ExpiresAt: e.Clock.Now().Add(400 * 24 * time.Hour)},
		"owner user": {Name: "ci", Owner: "frank", Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:read")}, ExpiresAt: exp},
	} {
		_, _, err := e.Svc.CreateToken(erin, in)
		if name == "owner user" {
			if !errors.Is(err, core.ErrForbidden) {
				t.Errorf("%s: err = %v, want ErrForbidden", name, err)
			}
			continue
		}
		if !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}

	tok, secret, err := e.Svc.CreateToken(erin, auth.TokenInput{
		Name: "npm ci", Scopes: []core.Privilege{core.MustParsePrivilege("repo:npm-*:read")}, ExpiresAt: exp,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, core.TokenPrefix) || tok.Prefix != secret[:8] || tok.Owner != "erin" {
		t.Fatalf("token = %+v secret %q", tok, secret)
	}
	var stored []byte
	if err := e.Pool.QueryRow(ctx, `SELECT hash FROM tokens WHERE id = $1`, tok.ID).Scan(&stored); err != nil || len(stored) != 32 || strings.Contains(string(stored), secret) {
		t.Fatalf("stored hash = %x (err=%v)", stored, err)
	}

	// Bearer, and Basic with any username.
	for _, c := range []core.Credentials{{Token: secret}, {Username: "erin", Password: secret}, {Username: "whatever", Password: secret}} {
		p, err := e.Svc.Authenticate(ctx, c)
		if err != nil {
			t.Fatalf("authenticate %+v: %v", c, err)
		}
		if p.Name != "erin" || p.TokenID != tok.ID || p.Source != auth.SourceToken || len(p.Scopes) != 1 {
			t.Fatalf("principal = %+v", p)
		}
		az := e.Svc.Authorizer()
		if err := az.Authorize(ctx, p, core.ActionRead, core.RepoResource("npm-hosted")); err != nil {
			t.Fatalf("scoped read: %v", err)
		}
		if err := az.Authorize(ctx, p, core.ActionWrite, core.RepoResource("npm-hosted")); !errors.Is(err, core.ErrForbidden) {
			t.Fatalf("scope must narrow Publisher write: %v", err)
		}
	}

	// last_used_at is written asynchronously.
	e.Svc.FlushUsage(ctx)
	page, err := e.Svc.ListTokens(erin, auth.TokenQuery{})
	if err != nil || len(page.Items) != 1 || page.Items[0].LastUsedAt == nil {
		t.Fatalf("list = %+v, %v", page, err)
	}

	// Others cannot list or revoke it.
	frank := e.As(t, "frank", pw)
	if _, err := e.Svc.ListTokens(frank, auth.TokenQuery{Owner: "erin"}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("frank lists erin's tokens: %v", err)
	}
	if err := e.Svc.RevokeToken(frank, tok.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("frank revokes erin's token: %v", err)
	}
	if pg, err := e.Svc.ListTokens(admin, auth.TokenQuery{Owner: "erin"}); err != nil || len(pg.Items) != 1 {
		t.Fatalf("admin list: %+v %v", pg, err)
	}

	// Expiry.
	e.Clock.Advance(25 * time.Hour)
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Token: secret}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("expired token accepted: %v", err)
	}
	e.Clock.Advance(-25 * time.Hour)

	// Revocation.
	if err := e.Svc.RevokeToken(erin, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Token: secret}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("revoked token accepted: %v", err)
	}
	if err := e.Svc.RevokeToken(erin, tok.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}

	// Garbage tokens are rejected and audited without the secret.
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Token: "hgr_notarealtoken"}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("garbage token: %v", err)
	}
	for _, ev := range e.Audit.Events(auth.AuditTokenAuthFailed) {
		if strings.Contains(ev.Target, secret) || len(ev.Target) > len("token:")+8 {
			t.Fatalf("audit leaks token: %+v", ev)
		}
	}
	if n := len(e.Audit.Events(auth.AuditTokenAuthFailed)); n != 3 {
		t.Fatalf("failed token auths audited = %d, want 3", n)
	}
	outcomes := func(action string) map[string]int {
		m := map[string]int{}
		for _, ev := range e.Audit.Events(action) {
			m[ev.Outcome]++
		}
		return m
	}
	if c := outcomes(auth.AuditTokenCreate); c[core.OutcomeSuccess] != 1 || c[core.OutcomeDenied] != 1 {
		t.Fatalf("token.create audit outcomes = %v, want 1 success and 1 denied (owner frank)", c)
	}
	if c := outcomes(auth.AuditTokenRevoke); c[core.OutcomeSuccess] != 1 {
		t.Fatalf("token.revoke audit outcomes = %v", c)
	}
}

func TestServiceAccountTokens(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "gina", "Publisher")
	if _, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "gitlab-ci", Kind: auth.KindServiceAccount, Roles: []string{"CI deployer"}}); err != nil {
		t.Fatal(err)
	}
	in := auth.TokenInput{Name: "deploy", Owner: "gitlab-ci", Scopes: []core.Privilege{core.MustParsePrivilege("repo:*:write")}, ExpiresAt: e.Clock.Now().Add(time.Hour)}
	if _, _, err := e.Svc.CreateToken(e.As(t, "gina", pw), in); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("non-admin creates SA token: %v", err)
	}
	if ev := e.Audit.Events(auth.AuditTokenCreate); len(ev) != 1 || ev[0].Outcome != core.OutcomeDenied {
		t.Fatalf("denial not audited: %+v", ev)
	}
	tok, secret, err := e.Svc.CreateToken(admin, in)
	if err != nil || tok.Owner != "gitlab-ci" {
		t.Fatalf("admin creates SA token: %+v, %v", tok, err)
	}
	p, err := e.Svc.Authenticate(context.Background(), core.Credentials{Username: "gitlab-ci-token", Password: secret})
	if err != nil || p.Kind != core.PrincipalServiceAccount || p.Name != "gitlab-ci" {
		t.Fatalf("SA token auth: %+v, %v", p, err)
	}
	if err := e.Svc.Authorizer().Authorize(context.Background(), p, core.ActionWrite, core.RepoResource("maven-hosted")); err != nil {
		t.Fatalf("SA write: %v", err)
	}
	if err := e.Svc.Authorizer().Authorize(context.Background(), p, core.ActionDelete, core.RepoResource("maven-hosted")); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("SA delete must be narrowed by scope: %v", err)
	}
}

func TestIssueSessionTokens(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "hank", "Publisher")
	hank := e.As(t, "hank", pw)
	ctx := context.Background()
	az := e.Svc.Authorizer()

	// Full rights when nothing narrows them.
	secret, exp, err := e.Svc.Issue(ctx, core.PrincipalFrom(hank), core.TokenRequest{Extra: map[string]any{"access": "pull"}})
	if err != nil || exp.Sub(e.Clock.Now()) != 15*time.Minute {
		t.Fatalf("issue = %v, %v", exp, err)
	}
	p, err := e.Svc.Authenticate(ctx, core.Credentials{Token: secret})
	if err != nil || p.Scopes != nil || p.Claims["access"] != "pull" {
		t.Fatalf("session token principal %+v, %v", p, err)
	}
	if err := az.Authorize(ctx, p, core.ActionWrite, core.RepoResource("any-repo")); err != nil {
		t.Fatalf("full rights: %v", err)
	}

	// Minted from a scoped PAT: the scopes intersect and revocation cascades.
	pat, patSecret, err := e.Svc.CreateToken(hank, auth.TokenInput{Name: "docker", Scopes: []core.Privilege{core.MustParsePrivilege("repo:docker-*:write")}, ExpiresAt: e.Clock.Now().Add(10 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	patP, err := e.Svc.Authenticate(ctx, core.Credentials{Token: patSecret})
	if err != nil {
		t.Fatal(err)
	}
	child, childExp, err := e.Svc.Issue(ctx, patP, core.TokenRequest{
		Scopes: []core.Privilege{core.MustParsePrivilege("repo:docker-hosted:write"), core.MustParsePrivilege("repo:npm-hosted:write")},
		TTL:    time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !childExp.Equal(pat.ExpiresAt) {
		t.Fatalf("child expiry %v not capped at parent %v", childExp, pat.ExpiresAt)
	}
	cp, err := e.Svc.Authenticate(ctx, core.Credentials{Token: child})
	if err != nil {
		t.Fatal(err)
	}
	if err := az.Authorize(ctx, cp, core.ActionWrite, core.RepoResource("docker-hosted")); err != nil {
		t.Fatalf("intersection keeps docker-hosted: %v", err)
	}
	if err := az.Authorize(ctx, cp, core.ActionWrite, core.RepoResource("npm-hosted")); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("intersection must drop npm-hosted: %v", err)
	}
	// Disjoint request: a token that can do nothing.
	none, _, err := e.Svc.Issue(ctx, patP, core.TokenRequest{Scopes: []core.Privilege{core.MustParsePrivilege("repo:maven-hosted:read")}})
	if err != nil {
		t.Fatal(err)
	}
	np, err := e.Svc.Authenticate(ctx, core.Credentials{Token: none})
	if err != nil || np.Scopes == nil || len(np.Scopes) != 0 {
		t.Fatalf("empty-scope token principal %+v, %v", np, err)
	}
	if err := az.Authorize(ctx, np, core.ActionRead, core.RepoResource("maven-hosted")); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("empty scopes must grant nothing: %v", err)
	}
	if err := e.Svc.RevokeToken(hank, pat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Token: child}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("child of revoked token accepted: %v", err)
	}
	if _, _, err := e.Svc.Issue(ctx, core.Anonymous(), core.TokenRequest{}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("issue for anonymous: %v", err)
	}
}

func TestSessions(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	u := mustCreateUser(t, e, admin, "ivy", "Reader")
	ctx := core.WithRequestInfo(context.Background(), core.RequestInfo{RemoteIP: "192.0.2.7", UserAgent: "test"})
	p, err := e.Svc.Login(ctx, "ivy", pw)
	if err != nil {
		t.Fatal(err)
	}
	secret, sess, err := e.Svc.CreateSession(ctx, p, false)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ExpiresAt.Sub(e.Clock.Now()) != 12*time.Hour || sess.CSRFToken == "" {
		t.Fatalf("session = %+v", sess)
	}
	sp, got, err := e.Svc.SessionPrincipal(ctx, secret)
	if err != nil || sp.ID != u.ID || got.ID != sess.ID || !got.ValidCSRF(sess.CSRFToken) || got.ValidCSRF("x") || got.ValidCSRF("") {
		t.Fatalf("lookup = %+v %+v %v", sp, got, err)
	}
	if _, _, err := e.Svc.SessionPrincipal(ctx, "not-a-session"); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("unknown session: %v", err)
	}
	e.Clock.Advance(13 * time.Hour)
	if _, _, err := e.Svc.SessionPrincipal(ctx, secret); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("expired session: %v", err)
	}
	// Remember me lasts 30 days; disabling the user ends it.
	secret, sess, err = e.Svc.CreateSession(ctx, p, true)
	if err != nil || sess.ExpiresAt.Sub(e.Clock.Now()) != 30*24*time.Hour {
		t.Fatalf("remember session = %+v, %v", sess, err)
	}
	if _, err := e.Svc.UpdateUser(admin, u.ID, auth.UserInput{Username: "ivy", Kind: auth.KindUser, Disabled: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Svc.SessionPrincipal(ctx, secret); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("session of disabled user: %v", err)
	}
	if err := e.Svc.PurgeExpired(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUserManagement(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	adminUser := mustUser(t, e, "admin")
	mustCreateUser(t, e, admin, "jack", "Reader")
	for _, n := range []string{"kate", "liam", "mia"} {
		mustCreateUser(t, e, admin, n)
	}
	if _, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "jack", Kind: auth.KindUser}); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("duplicate: %v", err)
	}
	for name, in := range map[string]auth.UserInput{
		"bad name":     {Username: "bad name", Kind: auth.KindUser},
		"reserved":     {Username: "anonymous", Kind: auth.KindUser},
		"bad kind":     {Username: "x1", Kind: "robot"},
		"weak pw":      {Username: "x2", Kind: auth.KindUser, Password: ptr("short")},
		"sa password":  {Username: "x3", Kind: auth.KindServiceAccount, Password: ptr(pw)},
		"unknown role": {Username: "x4", Kind: auth.KindUser, Roles: []string{"Wizard"}},
		"bad email":    {Username: "x5", Kind: auth.KindUser, Email: ptr("not-an-email")},
	} {
		if _, err := e.Svc.CreateUser(admin, in); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	// Pagination and filters.
	pg, err := e.Svc.ListUsers(admin, auth.UserQuery{Limit: 2})
	if err != nil || len(pg.Items) != 2 || pg.NextCursor == "" || pg.Items[0].Username != "admin" {
		t.Fatalf("page 1 = %+v, %v", pg, err)
	}
	pg2, err := e.Svc.ListUsers(admin, auth.UserQuery{Limit: 2, Cursor: pg.NextCursor})
	if err != nil || len(pg2.Items) != 2 || pg2.Items[0].Username != "kate" {
		t.Fatalf("page 2 = %+v, %v", pg2, err)
	}
	if pg, _ := e.Svc.ListUsers(admin, auth.UserQuery{Q: "LIA"}); len(pg.Items) != 1 || pg.Items[0].Username != "liam" {
		t.Fatalf("search = %+v", pg.Items)
	}
	if _, err := e.Svc.ListUsers(admin, auth.UserQuery{Cursor: "%%%"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad cursor: %v", err)
	}

	// Non-admins are refused and the refusal is audited.
	jack := e.As(t, "jack", pw)
	jackUser := mustUser(t, e, "jack")
	if _, err := e.Svc.ListUsers(jack, auth.UserQuery{}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("reader lists users: %v", err)
	}
	if _, err := e.Svc.CreateUser(jack, auth.UserInput{Username: "evil", Kind: auth.KindUser, Roles: []string{"Admin"}}); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("reader creates admin: %v", err)
	}
	if u, err := e.Svc.GetUser(jack, jackUser.ID); err != nil || u.Username != "jack" {
		t.Fatalf("self get: %v", err)
	}
	if ev := e.Audit.Events(auth.AuditUserCreate); ev[len(ev)-1].Outcome != core.OutcomeDenied {
		t.Fatalf("denied create not audited: %+v", ev[len(ev)-1])
	}

	// Role changes take effect on the next request.
	if _, err := e.Svc.UpdateUser(admin, jackUser.ID, auth.UserInput{Username: "jack", Kind: auth.KindUser, Roles: []string{"Publisher"}}); err != nil {
		t.Fatal(err)
	}
	jack = e.As(t, "jack", pw)
	if err := e.Svc.Authorizer().Authorize(jack, core.PrincipalFrom(jack), core.ActionWrite, core.RepoResource("npm-hosted")); err != nil {
		t.Fatalf("publisher after role change: %v", err)
	}
	if _, err := e.Svc.UpdateUser(admin, jackUser.ID, auth.UserInput{Username: "jack", Kind: auth.KindServiceAccount}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("kind change: %v", err)
	}

	// Guards: no self-disable, no self-delete, keep one admin.
	if _, err := e.Svc.UpdateUser(admin, adminUser.ID, auth.UserInput{Username: "admin", Kind: auth.KindUser, Disabled: ptr(true)}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("self disable: %v", err)
	}
	if err := e.Svc.DeleteUser(admin, adminUser.ID); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("self delete: %v", err)
	}
	if _, err := e.Svc.UpdateUser(admin, adminUser.ID, auth.UserInput{Username: "admin", Kind: auth.KindUser, Roles: []string{"Reader"}}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("demoting the last admin: %v", err)
	}
	if err := e.Svc.DeleteUser(admin, jackUser.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.GetUser(admin, jackUser.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("deleted user: %v", err)
	}
	if _, err := e.Svc.Authenticate(context.Background(), core.Credentials{Username: "jack", Password: pw}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("deleted user authenticates: %v", err)
	}
}

func TestRoles(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "nina")
	ctx := context.Background()
	role, err := e.Svc.CreateRole(admin, auth.RoleInput{
		Name: "npm team", Description: "npm publishers",
		Privileges: []core.Privilege{core.MustParsePrivilege("repo:npm-*:write"), core.MustParsePrivilege("repo:npm-*:read")},
	})
	if err != nil || len(role.Privileges) != 2 || role.Builtin {
		t.Fatalf("create role = %+v, %v", role, err)
	}
	if _, err := e.Svc.UpdateUser(admin, mustUser(t, e, "nina").ID, auth.UserInput{Username: "nina", Kind: auth.KindUser, Roles: []string{"npm team"}}); err != nil {
		t.Fatal(err)
	}
	nina := core.PrincipalFrom(e.As(t, "nina", pw))
	az := e.Svc.Authorizer()
	if err := az.Authorize(ctx, nina, core.ActionWrite, core.RepoResource("npm-hosted")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.UpdateRole(admin, "npm team", auth.RoleInput{Name: "npm team", Privileges: []core.Privilege{core.MustParsePrivilege("repo:npm-*:read")}}); err != nil {
		t.Fatal(err)
	}
	// The local cache was invalidated: write is gone immediately.
	if err := az.Authorize(ctx, nina, core.ActionWrite, core.RepoResource("npm-hosted")); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("after role update: %v", err)
	}
	roles, err := e.Svc.ListRoles(admin)
	if err != nil || len(roles) != 5 || !roles[0].Builtin {
		t.Fatalf("list roles = %d, %v", len(roles), err)
	}
	for _, r := range roles {
		if r.Name == "npm team" && r.MemberCount != 1 {
			t.Fatalf("member count = %d", r.MemberCount)
		}
	}
	if _, err := e.Svc.UpdateRole(admin, "Admin", auth.RoleInput{Name: "Admin"}); !errors.Is(err, auth.ErrBuiltinRole) {
		t.Fatalf("update builtin: %v", err)
	}
	if err := e.Svc.DeleteRole(admin, "Reader"); !errors.Is(err, auth.ErrBuiltinRole) {
		t.Fatalf("delete builtin: %v", err)
	}
	if _, err := e.Svc.CreateRole(admin, auth.RoleInput{Name: "sso", GroupMappings: []auth.GroupMapping{{Provider: "okta", Group: "devs"}}}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("unknown provider: %v", err)
	}
	if _, err := e.Pool.Exec(ctx, `INSERT INTO identity_providers (name, kind) VALUES ('okta', 'oidc')`); err != nil {
		t.Fatal(err)
	}
	if r, err := e.Svc.CreateRole(admin, auth.RoleInput{Name: "sso", GroupMappings: []auth.GroupMapping{{Provider: "okta", Group: "devs"}}}); err != nil || len(r.GroupMappings) != 1 {
		t.Fatalf("group mapping role = %+v, %v", r, err)
	}
	if got, err := e.Svc.RolesForGroups(ctx, "okta", []string{"devs", "other"}); err != nil || !slices.Equal(got, []string{"sso"}) {
		t.Fatalf("roles for groups = %v, %v", got, err)
	}
	if err := e.Svc.DeleteRole(admin, "npm team"); err != nil {
		t.Fatal(err)
	}
	if err := e.Svc.DeleteRole(admin, "npm team"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
