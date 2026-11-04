package auth_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/db"
)

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		pw, err := auth.GeneratePassword(auth.DefaultPasswordPolicy, "admin")
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != auth.GeneratedPasswordLength || strings.ContainsAny(pw, "0Oo1Il|ij") {
			t.Fatalf("password %q: wrong length or ambiguous characters", pw)
		}
		if err := auth.DefaultPasswordPolicy.Check("admin", pw); err != nil {
			t.Fatalf("password %q fails the policy: %v", pw, err)
		}
		seen[pw] = true
	}
	if len(seen) != 50 {
		t.Fatalf("only %d distinct passwords out of 50", len(seen))
	}
	strict := auth.PasswordPolicy{MinLength: 30, MaxLength: 64, MinClasses: 4}
	pw, err := auth.GeneratePassword(strict, "admin")
	if err != nil || len(pw) != 30 || strict.Check("admin", pw) != nil {
		t.Fatalf("strict policy: %q, %v", pw, err)
	}
}

// secondNode opens another pool on the same schema, as another server would.
func secondNode(t *testing.T, e *authtest.Env, opts ...authtest.Option) *authtest.Env {
	t.Helper()
	pool, err := db.Open(context.Background(), e.Pool.Config().ConnString(), db.PoolOptions{MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return authtest.NewOnPool(t, pool, auth.Config{}, e.Clock, opts...)
}

func TestInitialAdminCreatedOnceAcrossNodes(t *testing.T) {
	fileA := filepath.Join(t.TempDir(), "secrets", "initial-admin-password")
	fileB := filepath.Join(t.TempDir(), "secrets", "initial-admin-password")
	a := authtest.New(t, auth.Config{}, authtest.WithInitialPasswordFile(fileA))
	b := secondNode(t, a, authtest.WithInitialPasswordFile(fileB))

	var wg sync.WaitGroup
	results := make([]*auth.InitialAdmin, 2)
	errs := make([]error, 2)
	for i, env := range []*authtest.Env{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = env.Svc.EnsureInitialAdmin(context.Background())
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("errors: %v, %v", errs[0], errs[1])
	}
	if (results[0] == nil) == (results[1] == nil) {
		t.Fatalf("want exactly one node to create the administrator, got %+v and %+v", results[0], results[1])
	}
	created, file, other := results[0], fileA, fileB
	if created == nil {
		created, file, other = results[1], fileB, fileA
	}
	if created.Username != "admin" || created.FromFile || created.File != file || len(created.Password) != auth.GeneratedPasswordLength {
		t.Fatalf("created = %+v", created)
	}

	// The password file: mode 0600, holding the generated password.
	st, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(file); strings.TrimSpace(string(b)) != created.Password {
		t.Fatalf("file holds %q, want the generated password", b)
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the other node wrote its file too: %v", err)
	}

	var users int
	if err := a.Pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&users); err != nil || users != 1 {
		t.Fatalf("users = %d, %v", users, err)
	}
	u := mustUser(t, a, "admin")
	if !u.MustChangePassword || !slices.Contains(u.Roles, auth.AdminRole) {
		t.Fatalf("admin = %+v", u)
	}
	if ev := a.Audit.Events(auth.AuditUserBootstrap); len(ev) != 1 || ev[0].Details["password_source"] != "generated" {
		t.Fatalf("bootstrap audit = %+v", ev)
	}

	// Later starts change nothing.
	if again, err := a.Svc.EnsureInitialAdmin(context.Background()); err != nil || again != nil {
		t.Fatalf("second run = %+v, %v", again, err)
	}
}

func TestInitialAdminUsesPasswordFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "initial-admin-password")
	if err := os.WriteFile(file, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := authtest.New(t, auth.Config{}, authtest.WithInitialPasswordFile(file))
	ctx := context.Background()
	if _, err := e.Svc.EnsureInitialAdmin(ctx); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("weak password in the file: err = %v", err)
	}
	const given = "installer chosen passphrase 42"
	if err := os.WriteFile(file, []byte(given+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := e.Svc.EnsureInitialAdmin(ctx)
	if err != nil || a == nil || !a.FromFile || a.Password != "" {
		t.Fatalf("from file = %+v, %v", a, err)
	}
	p, err := e.Svc.Login(ctx, "admin", given)
	if err != nil || !p.PasswordChangeRequired {
		t.Fatalf("login with the file password: %+v, %v", p, err)
	}
	if b, _ := os.ReadFile(file); string(b) != given+"\n" {
		t.Fatalf("file rewritten: %q", b)
	}
	// Changing the password deletes the file.
	if err := e.Svc.ChangePassword(core.WithPrincipal(ctx, p), given, "a brand new passphrase", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still there after the change: %v", err)
	}
}

func TestInitialAdminSkippedWithUsers(t *testing.T) {
	file := filepath.Join(t.TempDir(), "initial-admin-password")
	e := authtest.New(t, auth.Config{}, authtest.WithInitialPasswordFile(file))
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "alice")
	if a, err := e.Svc.EnsureInitialAdmin(context.Background()); err != nil || a != nil {
		t.Fatalf("with users = %+v, %v", a, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file written although users exist: %v", err)
	}
}

func TestRestrictedUntilPasswordChanged(t *testing.T) {
	file := filepath.Join(t.TempDir(), "initial-admin-password")
	e := authtest.New(t, auth.Config{}, authtest.WithInitialPasswordFile(file))
	ctx := context.Background()
	a, err := e.Svc.EnsureInitialAdmin(ctx)
	if err != nil || a == nil {
		t.Fatalf("initial admin: %+v, %v", a, err)
	}
	initial := a.Password

	// Interactive sign-in works but gives a restricted principal.
	p, err := e.Svc.Login(ctx, "admin", initial)
	if err != nil || !p.PasswordChangeRequired {
		t.Fatalf("login = %+v, %v", p, err)
	}
	pctx := core.WithPrincipal(ctx, p)
	if err := e.Svc.Authorizer().Authorize(ctx, p, core.ActionRead, core.SystemResource(core.SystemUsers)); !errors.Is(err, auth.ErrPasswordChangeRequired) {
		t.Fatalf("authorize = %v, want ErrPasswordChangeRequired", err)
	}
	if privs, err := e.Svc.Authorizer().Privileges(ctx, p); err != nil || len(privs) != 0 {
		t.Fatalf("privileges = %v, %v", privs, err)
	}
	// Package clients (Basic) are refused; the API gets the restricted principal.
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Username: "admin", Password: initial}); !errors.Is(err, auth.ErrPasswordChangeRequired) {
		t.Fatalf("basic authenticate = %v", err)
	}
	if rp, err := e.Svc.AuthenticateRequest(ctx, core.Credentials{Username: "admin", Password: initial}); err != nil || !rp.PasswordChangeRequired {
		t.Fatalf("authenticate request = %+v, %v", rp, err)
	}
	// No tokens of any kind.
	if _, _, err := e.Svc.CreateToken(pctx, auth.TokenInput{Name: "ci", Scopes: []core.Privilege{core.RepoPrivilege("*", core.ActionRead)},
		ExpiresAt: e.Clock.Now().Add(24 * time.Hour)}); !errors.Is(err, auth.ErrPasswordChangeRequired) {
		t.Fatalf("create token = %v", err)
	}
	// Even for a principal built without the flag (the CLI acts like this).
	plain := *p
	plain.PasswordChangeRequired = false
	if _, _, err := e.Svc.CreateToken(core.WithPrincipal(ctx, &plain), auth.TokenInput{Name: "ci", Scopes: []core.Privilege{core.RepoPrivilege("*", core.ActionRead)},
		ExpiresAt: e.Clock.Now().Add(24 * time.Hour)}); !errors.Is(err, auth.ErrPasswordChangeRequired) {
		t.Fatalf("create token without flag = %v", err)
	}
	if _, _, err := e.Svc.Issue(pctx, p, core.TokenRequest{Name: "docker login"}); !errors.Is(err, auth.ErrPasswordChangeRequired) {
		t.Fatalf("issue = %v", err)
	}

	// The given password cannot be kept.
	if err := e.Svc.ChangePassword(pctx, initial, initial, ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("reuse = %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("file gone before the change: %v", err)
	}
	if err := e.Svc.ChangePassword(pctx, initial, "my own long passphrase", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still there after the change: %v", err)
	}
	if u := mustUser(t, e, "admin"); u.MustChangePassword {
		t.Fatal("flag not cleared")
	}
	ev := e.Audit.Events(auth.AuditUserPasswordChanged)
	if len(ev) != 1 || ev[0].Details["reason"] != "first_login" || ev[0].Actor != "admin" {
		t.Fatalf("audit = %+v", ev)
	}
	p, err = e.Svc.Login(ctx, "admin", "my own long passphrase")
	if err != nil || p.PasswordChangeRequired {
		t.Fatalf("login after the change = %+v, %v", p, err)
	}
	if err := e.Svc.Authorizer().Authorize(ctx, p, core.ActionRead, core.SystemResource(core.SystemUsers)); err != nil {
		t.Fatalf("authorize after the change = %v", err)
	}
	if _, err := e.Svc.Login(ctx, "admin", initial); err == nil {
		t.Fatal("the initial password still works")
	}
}

func TestBootstrapAdminRequiresChange(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	ctx := context.Background()
	if _, err := e.Svc.EnsureBootstrapAdmin(ctx, "root", "configured passphrase", true); err != nil {
		t.Fatal(err)
	}
	if u := mustUser(t, e, "root"); !u.MustChangePassword {
		t.Fatal("bootstrap administrator not flagged")
	}
	if ev := e.Audit.Events(auth.AuditUserBootstrap); len(ev) != 1 || ev[0].Details["require_change"] != true {
		t.Fatalf("audit = %+v", ev)
	}
}

func TestRequirePasswordChange(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	alice := mustCreateUser(t, e, admin, "alice", "Reader")
	bob := mustCreateUser(t, e, admin, "bob")
	ctx := context.Background()

	// RBAC: a reader cannot, nobody can flag themself.
	asAlice := e.As(t, "alice", pw)
	if _, err := e.Svc.RequirePasswordChange(asAlice, bob.ID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("reader = %v, want forbidden", err)
	}
	if ev := e.Audit.Events(auth.AuditUserPasswordRequire); len(ev) != 1 || ev[0].Outcome != core.OutcomeDenied {
		t.Fatalf("denied audit = %+v", ev)
	}
	if _, err := e.Svc.RequirePasswordChange(admin, core.PrincipalFrom(admin).ID); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("self = %v", err)
	}
	u, err := e.Svc.RequirePasswordChange(admin, alice.ID)
	if err != nil || !u.MustChangePassword {
		t.Fatalf("require = %+v, %v", u, err)
	}
	// Idempotent; audited once.
	if _, err := e.Svc.RequirePasswordChange(admin, alice.ID); err != nil {
		t.Fatal(err)
	}
	if ev := e.Audit.Events(auth.AuditUserPasswordRequire); len(ev) != 2 || ev[1].Actor != "admin" || ev[1].Target != "user:alice" {
		t.Fatalf("audit = %+v", ev)
	}
	// Service accounts have no password.
	sa, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "ci", Kind: auth.KindServiceAccount})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.RequirePasswordChange(admin, sa.ID); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("service account = %v", err)
	}
	// Alice is restricted from her next request on.
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Username: "alice", Password: pw}); !errors.Is(err, auth.ErrPasswordChangeRequired) {
		t.Fatalf("alice basic = %v", err)
	}
	p, err := e.Svc.Login(ctx, "alice", pw)
	if err != nil || !p.PasswordChangeRequired {
		t.Fatalf("alice login = %+v, %v", p, err)
	}
	if err := e.Svc.ChangePassword(core.WithPrincipal(ctx, p), pw, "a fresh secret phrase", ""); err != nil {
		t.Fatal(err)
	}
	if p, err := e.Svc.Login(ctx, "alice", "a fresh secret phrase"); err != nil || p.PasswordChangeRequired {
		t.Fatalf("after change = %+v, %v", p, err)
	}
}

func TestResetPasswordOneTime(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "alice")
	ctx := context.Background()
	_, sess, _ := login(t, e, "alice", pw, "192.0.2.1")

	password, u, err := e.Svc.ResetPasswordOneTime(ctx, "alice")
	if err != nil || len(password) != auth.GeneratedPasswordLength || !u.MustChangePassword {
		t.Fatalf("reset = %q, %+v, %v", password, u, err)
	}
	if _, err := e.Svc.Login(ctx, "alice", pw); err == nil {
		t.Fatal("old password still works")
	}
	p, err := e.Svc.Login(ctx, "alice", password)
	if err != nil || !p.PasswordChangeRequired {
		t.Fatalf("login with one-time password = %+v, %v", p, err)
	}
	var sessions int
	if err := e.Pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id = $1`, sess.ID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("old session kept: %d, %v", sessions, err)
	}
	if ev := e.Audit.Events(auth.AuditUserPassword); len(ev) == 0 || ev[len(ev)-1].Actor != auth.SystemActor || ev[len(ev)-1].Details["reset_by"] != "cli" {
		t.Fatalf("audit = %+v", ev)
	}
	if _, _, err := e.Svc.ResetPasswordOneTime(ctx, "nobody"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown user = %v", err)
	}
}
