package auth_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

const issuer = "https://sso.example.com/realms/acme"

func oidcConfig() auth.Config {
	return auth.Config{OIDC: auth.OIDCConfig{
		Enabled: true, IssuerURL: issuer, ClientID: "hangar", AutoCreate: true,
		GroupRoles:   map[string][]string{"devs": {"Publisher"}, "hangar-admins": {"Admin"}},
		DefaultRoles: []string{"Reader"},
	}}
}

func oidcUser(sub, name string, groups ...string) auth.ExternalIdentity {
	return auth.ExternalIdentity{
		Source: auth.SourceOIDC, Issuer: issuer, Subject: sub, Username: name,
		Email: name + "@example.com", DisplayName: strings.ToUpper(name), Groups: groups,
	}
}

func wantExternal(t *testing.T, err error, code string) {
	t.Helper()
	var xe *auth.ExternalError
	if !errors.As(err, &xe) || xe.Code != code {
		t.Fatalf("err = %v, want ExternalError %s", err, code)
	}
}

func TestExternalProvisionAndRoleSync(t *testing.T) {
	e := authtest.New(t, oidcConfig())
	admin := e.Admin(t)
	ctx := context.Background()

	p, err := e.Svc.SignInExternal(ctx, oidcUser("sub-1", "jdoe", "DEVS"), auth.MethodOIDC)
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != auth.SourceOIDC || !slices.Equal(p.Roles, []string{"Publisher", "Reader"}) {
		t.Fatalf("principal = %+v (groups match case-insensitively)", p)
	}
	u, err := e.Svc.GetUserByName(ctx, "jdoe")
	if err != nil {
		t.Fatal(err)
	}
	if u.Source != auth.SourceOIDC || u.PasswordSet || u.Email != "jdoe@example.com" || u.DisplayName != "JDOE" {
		t.Fatalf("user = %+v", u)
	}
	if ev := e.Audit.Events(auth.AuditUserCreate); len(ev) != 1 || ev[0].Details["source"] != auth.SourceOIDC {
		t.Fatalf("user.create audit = %+v", ev)
	}
	if ev := e.Audit.Events(auth.AuditLogin); len(ev) != 1 || ev[0].Details["method"] != auth.MethodOIDC {
		t.Fatalf("auth.login audit = %+v", ev)
	}

	// An administrator binds a role by hand; it survives every re-sync.
	if err := e.Svc.BindRole(admin, "CI deployer", u.ID); err != nil {
		t.Fatal(err)
	}
	p, err = e.Svc.SignInExternal(ctx, oidcUser("sub-1", "jdoe", "hangar-admins"), auth.MethodOIDC)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Admin", "CI deployer", "Reader"}; !slices.Equal(p.Roles, want) {
		t.Fatalf("after group change roles = %v, want %v", p.Roles, want)
	}
	// A mapped role bound by hand becomes manual and stays without the group.
	if err := e.Svc.BindRole(admin, "Admin", u.ID); err != nil {
		t.Fatal(err)
	}
	p, err = e.Svc.SignInExternal(ctx, oidcUser("sub-1", "jdoe"), auth.MethodOIDC)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Admin", "CI deployer", "Reader"}; !slices.Equal(p.Roles, want) {
		t.Fatalf("manual Admin lost: roles = %v, want %v", p.Roles, want)
	}
	if err := e.Svc.UnbindRole(admin, "Admin", u.ID); err != nil {
		t.Fatal(err)
	}
	// Without any group only the default role is mapped.
	p, err = e.Svc.SignInExternal(ctx, oidcUser("sub-1", "jdoe"), auth.MethodOIDC)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"CI deployer", "Reader"}; !slices.Equal(p.Roles, want) {
		t.Fatalf("roles = %v, want %v", p.Roles, want)
	}
	// Changing the configuration drops roles it no longer maps.
	cfg := oidcConfig()
	cfg.OIDC.DefaultRoles = nil
	e2 := authtest.NewOnPool(t, e.Pool, cfg, e.Clock)
	p, err = e2.Svc.SignInExternal(ctx, oidcUser("sub-1", "jdoe"), auth.MethodOIDC)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"CI deployer"}; !slices.Equal(p.Roles, want) {
		t.Fatalf("after config change roles = %v, want %v", p.Roles, want)
	}
	if ev := e2.Audit.Events(auth.AuditLogin); len(ev) != 1 || !slices.Equal(ev[0].Details["roles_removed"].([]string), []string{"Reader"}) {
		t.Fatalf("auth.login audit = %+v", ev)
	}
}

func TestExternalGroupPaths(t *testing.T) {
	cfg := oidcConfig()
	cfg.OIDC.AllowedGroups = []string{"/it/devops", "staff"}
	e := authtest.New(t, cfg)
	ctx := context.Background()
	// Keycloak full paths match names with or without the leading "/".
	p, err := e.Svc.SignInExternal(ctx, oidcUser("sub-1", "ops1", "/it/devops", "/devs"), auth.MethodOIDC)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Publisher", "Reader"}; !slices.Equal(p.Roles, want) {
		t.Fatalf("roles = %v, want %v", p.Roles, want)
	}
	if _, err := e.Svc.SignInExternal(ctx, oidcUser("sub-2", "ops2", "/staff"), auth.MethodOIDC); err != nil {
		t.Fatalf("allowed group by path: %v", err)
	}
	_, err = e.Svc.SignInExternal(ctx, oidcUser("sub-3", "ops3", "/it"), auth.MethodOIDC)
	wantExternal(t, err, auth.CodeGroupDenied)
}

func TestExternalGroupMappingsFromRoles(t *testing.T) {
	e := authtest.New(t, oidcConfig())
	admin := e.Admin(t)
	if _, err := e.Svc.CreateRole(admin, auth.RoleInput{
		Name: "Docs team", Privileges: []core.Privilege{core.RepoPrivilege("docs-*", core.ActionWrite)},
		GroupMappings: []auth.GroupMapping{{Provider: "oidc", Group: "docs"}},
	}); err != nil {
		t.Fatalf("role with an oidc group mapping: %v", err)
	}
	p, err := e.Svc.SignInExternal(context.Background(), oidcUser("sub-9", "writer", "/docs"), auth.MethodOIDC)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Docs team", "Reader"}; !slices.Equal(p.Roles, want) {
		t.Fatalf("roles = %v, want %v", p.Roles, want)
	}
}

func TestExternalNeverTakesOverAccounts(t *testing.T) {
	e := authtest.New(t, oidcConfig())
	admin := e.Admin(t)
	ctx := context.Background()
	local := mustCreateUser(t, e, admin, "jdoe", "Reader")

	_, err := e.Svc.SignInExternal(ctx, oidcUser("sub-1", "jdoe", "hangar-admins"), auth.MethodOIDC)
	wantExternal(t, err, auth.CodeAccountConflict)
	u, _ := e.Svc.GetUserByName(ctx, "jdoe")
	if u.ID != local.ID || u.Source != auth.SourceLocal || slices.Contains(u.Roles, "Admin") {
		t.Fatalf("local user changed: %+v", u)
	}
	if _, err := e.Svc.Login(ctx, "jdoe", pw); err != nil {
		t.Fatalf("local login after collision: %v", err)
	}
	if ev := e.Audit.Events(auth.AuditLoginFailed); len(ev) == 0 || ev[len(ev)-1].Details["reason"] != auth.CodeAccountConflict {
		t.Fatalf("failure audit = %+v", ev)
	}

	// A second identity of the same provider cannot take an existing name.
	if _, err := e.Svc.SignInExternal(ctx, oidcUser("sub-2", "alice"), auth.MethodOIDC); err != nil {
		t.Fatal(err)
	}
	_, err = e.Svc.SignInExternal(ctx, oidcUser("sub-3", "alice"), auth.MethodOIDC)
	wantExternal(t, err, auth.CodeAccountConflict)
	// Another issuer is another identity, too.
	other := oidcUser("sub-2", "alice")
	other.Issuer = "https://other.example.com"
	_, err = e.Svc.SignInExternal(ctx, other, auth.MethodOIDC)
	wantExternal(t, err, auth.CodeAccountConflict)

	// Service accounts are never signed in.
	if _, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "ci-bot", Kind: auth.KindServiceAccount}); err != nil {
		t.Fatal(err)
	}
	_, err = e.Svc.SignInExternal(ctx, oidcUser("sub-4", "ci-bot"), auth.MethodOIDC)
	wantExternal(t, err, auth.CodeAccountConflict)
}

func TestExternalRenameFollowsSubject(t *testing.T) {
	e := authtest.New(t, oidcConfig())
	ctx := context.Background()
	p1, err := e.Svc.SignInExternal(ctx, oidcUser("sub-1", "jdoe"), auth.MethodOIDC)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := e.Svc.SignInExternal(ctx, oidcUser("sub-1", "jane.doe"), auth.MethodOIDC)
	if err != nil {
		t.Fatal(err)
	}
	if p2.ID != p1.ID || p2.Name != "jane.doe" {
		t.Fatalf("rename: %s/%s -> %s/%s", p1.ID, p1.Name, p2.ID, p2.Name)
	}
	// A new name that is taken keeps the old one.
	if _, err := e.Svc.SignInExternal(ctx, oidcUser("sub-2", "bob"), auth.MethodOIDC); err != nil {
		t.Fatal(err)
	}
	p3, err := e.Svc.SignInExternal(ctx, oidcUser("sub-1", "bob"), auth.MethodOIDC)
	if err != nil || p3.ID != p1.ID || p3.Name != "jane.doe" {
		t.Fatalf("rename onto a taken name = %+v, %v", p3, err)
	}
}

func TestExternalEmailCollision(t *testing.T) {
	e := authtest.New(t, oidcConfig())
	admin := e.Admin(t)
	mustCreateUser(t, e, admin, "carol") // carol@example.test
	id := oidcUser("sub-1", "carol2")
	id.Email = "CAROL@example.test"
	if _, err := e.Svc.SignInExternal(context.Background(), id, auth.MethodOIDC); err != nil {
		t.Fatalf("email of another user must not block sign-in: %v", err)
	}
	if u, _ := e.Svc.GetUserByName(context.Background(), "carol2"); u.Email != "" {
		t.Fatalf("email stored although taken: %q", u.Email)
	}
}

func TestExternalAdmission(t *testing.T) {
	cfg := oidcConfig()
	cfg.OIDC.AllowedGroups = []string{"staff"}
	e := authtest.New(t, cfg)
	admin := e.Admin(t)
	ctx := context.Background()

	_, err := e.Svc.SignInExternal(ctx, oidcUser("sub-1", "guest", "devs"), auth.MethodOIDC)
	wantExternal(t, err, auth.CodeGroupDenied)
	if _, err := e.Svc.GetUserByName(ctx, "guest"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("denied user was created: %v", err)
	}
	p, err := e.Svc.SignInExternal(ctx, oidcUser("sub-2", "member", "Staff"), auth.MethodOIDC)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Svc.SignInExternal(ctx, oidcUser("sub-3", "bad name!", "staff"), auth.MethodOIDC)
	wantExternal(t, err, auth.CodeInvalidProfile)

	if _, err := e.Svc.SetUserDisabled(admin, p.ID, true); err != nil {
		t.Fatal(err)
	}
	_, err = e.Svc.SignInExternal(ctx, oidcUser("sub-2", "member", "staff"), auth.MethodOIDC)
	wantExternal(t, err, auth.CodeAccountDisabled)

	cfg.OIDC.AutoCreate = false
	e2 := authtest.NewOnPool(t, e.Pool, cfg, e.Clock)
	_, err = e2.Svc.SignInExternal(ctx, oidcUser("sub-4", "newbie", "staff"), auth.MethodOIDC)
	wantExternal(t, err, auth.CodeNotProvisioned)
}

func TestExternalConfigNamesUnknownRole(t *testing.T) {
	pool := authtest.Pool(t, true)
	cfg := oidcConfig()
	cfg.OIDC.GroupRoles["ops"] = []string{"Operators"}
	cfg.Argon2 = authtest.FastArgon2
	_, err := auth.New(pool, auth.Options{Config: cfg, Audit: discardAudit{}})
	if err == nil || !strings.Contains(err.Error(), `role "Operators" does not exist`) {
		t.Fatalf("err = %v", err)
	}
}

type discardAudit struct{}

func (discardAudit) Record(context.Context, core.AuditEvent) error { return nil }

// fakeDirectory accepts the passwords in users and counts calls.
type fakeDirectory struct {
	mu     sync.Mutex
	users  map[string]string // username -> password
	groups map[string][]string
	calls  int
	down   bool
}

func (d *fakeDirectory) Authenticate(_ context.Context, username, password string) (*auth.ExternalIdentity, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.down {
		return nil, errors.New("connection refused")
	}
	name := strings.ToLower(username)
	if pw, ok := d.users[name]; !ok || pw != password || password == "" {
		return nil, core.ErrUnauthenticated
	}
	return &auth.ExternalIdentity{Source: auth.SourceLDAP, Username: name, Email: name + "@corp.example.com", Groups: d.groups[name]}, nil
}

func (d *fakeDirectory) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func ldapEnv(t *testing.T) (*authtest.Env, *fakeDirectory) {
	t.Helper()
	dir := &fakeDirectory{
		users:  map[string]string{"jdoe": "directory password", "admin": "directory password"},
		groups: map[string][]string{"jdoe": {"CN=Hangar Publishers,OU=Groups,DC=example,DC=com", "Hangar Publishers"}},
	}
	cfg := auth.Config{
		Lockout: auth.Lockout{MaxAttempts: 3},
		LDAP: auth.LDAPConfig{
			Enabled: true, URL: "ldaps://dc1.example.com", UserBaseDN: "DC=example,DC=com", AutoCreate: true,
			GroupRoles: map[string][]string{"hangar publishers": {"Publisher"}}, DefaultRoles: []string{"Reader"},
		},
	}
	return authtest.New(t, cfg, authtest.WithDirectory(dir)), dir
}

func TestLDAPLogin(t *testing.T) {
	e, dir := ldapEnv(t)
	ctx := context.Background()
	p, err := e.Svc.LoginLDAP(ctx, "JDoe", "directory password")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "jdoe" || p.Source != auth.SourceLDAP || !slices.Equal(p.Roles, []string{"Publisher", "Reader"}) {
		t.Fatalf("principal = %+v", p)
	}
	if ev := e.Audit.Events(auth.AuditLogin); len(ev) != 1 || ev[0].Details["method"] != auth.MethodLDAP {
		t.Fatalf("login audit = %+v", ev)
	}
	// No local password: local login and password changes are refused.
	if _, err := e.Svc.Login(ctx, "jdoe", "directory password"); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("local login of an LDAP user: %v", err)
	}
	if err := e.Svc.ChangePassword(core.WithPrincipal(ctx, p), "directory password", "a brand new passphrase", ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("change password of an LDAP user: %v", err)
	}
	// An empty password never reaches the directory.
	before := dir.callCount()
	if _, err := e.Svc.LoginLDAP(ctx, "jdoe", ""); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("empty password: %v", err)
	}
	if dir.callCount() != before {
		t.Fatal("empty password was sent to the directory")
	}
}

func TestLDAPLockout(t *testing.T) {
	e, dir := ldapEnv(t)
	ctx := context.Background()
	if _, err := e.Svc.LoginLDAP(ctx, "jdoe", "directory password"); err != nil {
		t.Fatal(err)
	}
	var locked *auth.LockedError
	for i := range 3 {
		_, err := e.Svc.LoginLDAP(ctx, "jdoe", "wrong")
		if i < 2 && (!errors.Is(err, core.ErrUnauthenticated) || errors.As(err, &locked)) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		if i == 2 && !errors.As(err, &locked) {
			t.Fatalf("attempt 3 should lock: %v", err)
		}
	}
	before := dir.callCount()
	if _, err := e.Svc.LoginLDAP(ctx, "jdoe", "directory password"); !errors.As(err, &locked) {
		t.Fatalf("locked account signed in: %v", err)
	}
	if dir.callCount() != before {
		t.Fatal("a locked account was checked against the directory")
	}
}

func TestLDAPBasicAuth(t *testing.T) {
	e, dir := ldapEnv(t)
	admin := e.Admin(t) // local "admin" also exists in the directory
	ctx := context.Background()

	// Basic auth provisions the LDAP user and is cached afterwards.
	p, err := e.Svc.Authenticate(ctx, core.Credentials{Username: "jdoe", Password: "directory password"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != auth.SourceLDAP || !slices.Contains(p.Roles, "Publisher") {
		t.Fatalf("principal = %+v", p)
	}
	calls := dir.callCount()
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Username: "jdoe", Password: "directory password"}); err != nil {
		t.Fatal(err)
	}
	if dir.callCount() != calls {
		t.Fatal("Basic cache not used for the LDAP user")
	}
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Username: "jdoe", Password: "wrong"}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("wrong password: %v", err)
	}

	// A local user is checked locally even though the directory knows the name.
	calls = dir.callCount()
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Username: "admin", Password: authtest.AdminPassword}); err != nil {
		t.Fatalf("local admin: %v", err)
	}
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Username: "admin", Password: "directory password"}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("directory password accepted for the local admin: %v", err)
	}
	if dir.callCount() != calls {
		t.Fatal("local user was checked against the directory")
	}
	// And realm ldap cannot take the local account over.
	_, err = e.Svc.LoginLDAP(ctx, "admin", "directory password")
	wantExternal(t, err, auth.CodeAccountConflict)
	_ = admin
}

func TestLDAPDirectoryDown(t *testing.T) {
	e, dir := ldapEnv(t)
	dir.down = true
	_, err := e.Svc.LoginLDAP(context.Background(), "jdoe", "directory password")
	if !errors.Is(err, auth.ErrDirectoryUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestLDAPRequiresDirectory(t *testing.T) {
	pool := authtest.Pool(t, true)
	cfg := auth.Config{Argon2: authtest.FastArgon2, LDAP: auth.LDAPConfig{Enabled: true, URL: "ldaps://dc1.example.com", UserBaseDN: "DC=example,DC=com"}}
	if _, err := auth.New(pool, auth.Options{Config: cfg, Audit: discardAudit{}}); err == nil {
		t.Fatal("LDAP enabled without a directory was accepted")
	}
}
