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

// TestEscalationMatrix: system:users:write and system:tokens:write never let
// a caller hand out, or take over an account with, more than the caller
// holds. Each row is one caller trying one operation.
func TestEscalationMatrix(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	for _, r := range []auth.RoleInput{
		{Name: "Helpdesk", Privileges: privs("system:users:read", "system:users:write", "system:users:delete")},
		{Name: "TokenOps", Privileges: privs("system:tokens:write", "repo:*:read")},
		{Name: "Docs", Privileges: privs("repo:docs-*:read")},
	} {
		if _, err := e.Svc.CreateRole(admin, r); err != nil {
			t.Fatal(err)
		}
	}
	adminID := mustUser(t, e, "admin").ID
	admin2 := mustCreateUser(t, e, admin, "admin2", "Admin")
	help := mustCreateUser(t, e, admin, "help", "Helpdesk")
	helpReader := mustCreateUser(t, e, admin, "helpreader", "Helpdesk", "Reader")
	reader := mustCreateUser(t, e, admin, "reader", "Reader")
	plain := mustCreateUser(t, e, admin, "plain")
	tokOps := mustCreateUser(t, e, admin, "tokops", "TokenOps")
	_, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "sa-publish", Kind: auth.KindServiceAccount, Roles: []string{"Publisher"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.CreateUser(admin, auth.UserInput{Username: "sa-read", Kind: auth.KindServiceAccount, Roles: []string{"Reader"}}); err != nil {
		t.Fatal(err)
	}

	as := func(name string) context.Context {
		return core.WithPrincipal(context.Background(), principalOf(t, e, name))
	}
	asHelp, asHelpReader, asReader, asTokOps := as(help.Username), as(helpReader.Username), as(reader.Username), as(tokOps.Username)
	expires := time.Now().Add(time.Hour)
	const newPass = "a different passphrase"

	cases := []struct {
		name string
		ctx  context.Context
		op   func(ctx context.Context) error
		ok   bool
	}{
		// Passwords.
		{"helpdesk resets the admin's password", asHelp, func(c context.Context) error { return e.Svc.ResetPassword(c, adminID, newPass) }, false},
		{"helpdesk resets a reader's password", asHelp, func(c context.Context) error { return e.Svc.ResetPassword(c, reader.ID, newPass) }, false},
		{"helpdesk resets a plain user's password", asHelp, func(c context.Context) error { return e.Svc.ResetPassword(c, plain.ID, newPass) }, true},
		{"helpdesk+reader resets a reader's password", asHelpReader, func(c context.Context) error { return e.Svc.ResetPassword(c, reader.ID, newPass) }, true},
		{"helpdesk+reader resets the admin's password", asHelpReader, func(c context.Context) error { return e.Svc.ResetPassword(c, adminID, newPass) }, false},
		{"helpdesk sets the admin's password by update", asHelp, func(c context.Context) error {
			_, err := e.Svc.UpdateUser(c, adminID, auth.UserInput{Username: "admin", Kind: auth.KindUser, Password: ptr(newPass)})
			return err
		}, false},
		{"admin resets another admin's password", admin, func(c context.Context) error { return e.Svc.ResetPassword(c, admin2.ID, newPass) }, true},
		{"reader resets a plain user's password", asReader, func(c context.Context) error { return e.Svc.ResetPassword(c, plain.ID, newPass) }, false},

		// Disabling, enabling, deleting, requiring a change, ending sessions.
		{"helpdesk disables the admin", asHelp, func(c context.Context) error { _, err := e.Svc.SetUserDisabled(c, adminID, true); return err }, false},
		{"helpdesk enables a disabled admin", asHelp, func(c context.Context) error { _, err := e.Svc.SetUserDisabled(c, admin2.ID, false); return err }, false},
		{"helpdesk disables a plain user", asHelp, func(c context.Context) error { _, err := e.Svc.SetUserDisabled(c, plain.ID, true); return err }, true},
		{"helpdesk re-enables a plain user", asHelp, func(c context.Context) error { _, err := e.Svc.SetUserDisabled(c, plain.ID, false); return err }, true},
		{"helpdesk deletes the admin", asHelp, func(c context.Context) error { return e.Svc.DeleteUser(c, admin2.ID) }, false},
		{"helpdesk requires the admin to change password", asHelp, func(c context.Context) error { _, err := e.Svc.RequirePasswordChange(c, adminID); return err }, false},
		{"helpdesk ends the admin's sessions", asHelp, func(c context.Context) error { _, err := e.Svc.RevokeSessions(c, adminID, ""); return err }, false},

		// Role bindings.
		{"helpdesk binds Admin to itself", asHelp, func(c context.Context) error { return e.Svc.BindRole(c, auth.AdminRole, help.ID) }, false},
		{"helpdesk binds Reader to a plain user", asHelp, func(c context.Context) error { return e.Svc.BindRole(c, "Reader", plain.ID) }, false},
		{"helpdesk unbinds Admin from the admin", asHelp, func(c context.Context) error { return e.Svc.UnbindRole(c, auth.AdminRole, admin2.ID) }, false},
		{"helpdesk binds Helpdesk to a plain user", asHelp, func(c context.Context) error { return e.Svc.BindRole(c, "Helpdesk", plain.ID) }, true},
		{"helpdesk+reader binds Reader to a plain user", asHelpReader, func(c context.Context) error { return e.Svc.BindRole(c, "Reader", plain.ID) }, true},
		{"helpdesk+reader binds Docs (narrower) to a plain user", asHelpReader, func(c context.Context) error { return e.Svc.BindRole(c, "Docs", plain.ID) }, true},
		{"helpdesk gives itself Admin by update", asHelp, func(c context.Context) error {
			_, err := e.Svc.UpdateUser(c, help.ID, auth.UserInput{Username: "help", Kind: auth.KindUser, Roles: []string{"Helpdesk", auth.AdminRole}})
			return err
		}, false},
		{"admin binds Admin to a plain user", admin, func(c context.Context) error { return e.Svc.BindRole(c, auth.AdminRole, plain.ID) }, true},
		{"admin unbinds Admin from that user", admin, func(c context.Context) error { return e.Svc.UnbindRole(c, auth.AdminRole, plain.ID) }, true},

		// Roles.
		{"helpdesk creates a system:* role", asHelp, func(c context.Context) error {
			_, err := e.Svc.CreateRole(c, auth.RoleInput{Name: "Mine", Privileges: privs("system:*:*")})
			return err
		}, false},
		{"helpdesk creates a repo reader role", asHelp, func(c context.Context) error {
			_, err := e.Svc.CreateRole(c, auth.RoleInput{Name: "Mine", Privileges: privs("repo:*:read")})
			return err
		}, false},
		{"helpdesk creates a role within its rights", asHelp, func(c context.Context) error {
			_, err := e.Svc.CreateRole(c, auth.RoleInput{Name: "UserViewer", Privileges: privs("system:users:read")})
			return err
		}, true},
		{"helpdesk widens Docs to every repository", asHelp, func(c context.Context) error {
			_, err := e.Svc.UpdateRole(c, "Docs", auth.RoleInput{Name: "Docs", Privileges: privs("system:users:read")})
			return err
		}, false},
		{"helpdesk deletes TokenOps", asHelp, func(c context.Context) error {
			return e.Svc.DeleteRole(c, "TokenOps")
		}, false},

		// Accounts created with roles.
		{"helpdesk creates an Admin account", asHelp, func(c context.Context) error {
			_, err := e.Svc.CreateUser(c, auth.UserInput{Username: "sneaky", Kind: auth.KindUser, Password: ptr(pw), Roles: []string{auth.AdminRole}})
			return err
		}, false},
		{"helpdesk creates a Reader account", asHelp, func(c context.Context) error {
			_, err := e.Svc.CreateUser(c, auth.UserInput{Username: "sneaky", Kind: auth.KindUser, Password: ptr(pw), Roles: []string{"Reader"}})
			return err
		}, false},
		{"helpdesk creates an account without roles", asHelp, func(c context.Context) error {
			_, err := e.Svc.CreateUser(c, auth.UserInput{Username: "newbie", Kind: auth.KindUser, Password: ptr(pw)})
			return err
		}, true},
		{"helpdesk+reader creates a Reader account", asHelpReader, func(c context.Context) error {
			_, err := e.Svc.CreateUser(c, auth.UserInput{Username: "newreader", Kind: auth.KindUser, Password: ptr(pw), Roles: []string{"Reader"}})
			return err
		}, true},

		// Tokens for service accounts.
		{"tokens:write issues a token for a publishing service account", asTokOps, func(c context.Context) error {
			_, _, err := e.Svc.CreateToken(c, auth.TokenInput{Name: "ci", Owner: "sa-publish", Scopes: privs("repo:*:read"), ExpiresAt: expires})
			return err
		}, false},
		{"tokens:write issues a token for a reading service account", asTokOps, func(c context.Context) error {
			_, _, err := e.Svc.CreateToken(c, auth.TokenInput{Name: "ci", Owner: "sa-read", Scopes: privs("repo:*:read"), ExpiresAt: expires})
			return err
		}, true},
		{"admin issues a token for a publishing service account", admin, func(c context.Context) error {
			_, _, err := e.Svc.CreateToken(c, auth.TokenInput{Name: "ci", Owner: "sa-publish", Scopes: privs("repo:*:write"), ExpiresAt: expires})
			return err
		}, true},
	}
	// admin2 starts disabled for the "enable" row; a disabled Admin is
	// still an Admin.
	if _, err := e.Svc.SetUserDisabled(admin, admin2.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		err := c.op(c.ctx)
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: %v, want success", c.name, err)
		case !c.ok && !errors.Is(err, core.ErrForbidden):
			t.Errorf("%s: %v, want forbidden", c.name, err)
		}
	}

	// No take-over happened: the admin's password is
	// unchanged, the admin still holds Admin, and every refusal was audited.
	if _, err := e.Svc.Login(context.Background(), "admin", authtest.AdminPassword); err != nil {
		t.Fatalf("admin password changed: %v", err)
	}
	if u := mustUser(t, e, "help"); len(u.Roles) != 1 || u.Roles[0] != "Helpdesk" {
		t.Errorf("helpdesk roles = %v", u.Roles)
	}
	want := 0
	for _, c := range cases {
		if !c.ok && c.ctx != asReader {
			want++ // the reader lacks system:users:write and is refused before
		}
	}
	denied := 0
	for _, ev := range e.Audit.Events() {
		if ev.Outcome == core.OutcomeDenied && ev.Details["missing"] != nil {
			denied++
		}
	}
	if denied != want {
		t.Errorf("audited escalation denials = %d, want %d", denied, want)
	}
}

// TestPasswordSetByAnotherMustChange: a password another user set (reset or
// update) must be replaced at the next sign-in.
func TestPasswordSetByAnotherMustChange(t *testing.T) {
	e := authtest.New(t, auth.Config{})
	admin := e.Admin(t)
	u := mustCreateUser(t, e, admin, "erin", "Reader")
	if mustUser(t, e, "erin").MustChangePassword {
		t.Fatal("a new account must not start restricted")
	}
	if err := e.Svc.ResetPassword(admin, u.ID, "reset by the admin 1"); err != nil {
		t.Fatal(err)
	}
	p, err := e.Svc.Login(context.Background(), "erin", "reset by the admin 1")
	if err != nil || !p.PasswordChangeRequired {
		t.Fatalf("after reset: %+v, %v; want password change required", p, err)
	}
	ctx := core.WithPrincipal(context.Background(), p)
	if err := e.Svc.ChangePassword(ctx, "reset by the admin 1", "a passphrase of her own", ""); err != nil {
		t.Fatal(err)
	}
	if mustUser(t, e, "erin").MustChangePassword {
		t.Fatal("still restricted after choosing a password")
	}
	if _, err := e.Svc.UpdateUser(admin, u.ID, auth.UserInput{Username: "erin", Kind: auth.KindUser, Password: ptr("set by the admin 22")}); err != nil {
		t.Fatal(err)
	}
	if !mustUser(t, e, "erin").MustChangePassword {
		t.Fatal("a password set by update must be replaced too")
	}
}
