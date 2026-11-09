package auth

import (
	"context"
	"fmt"
	"slices"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Privilege escalation guard.
//
// system:users:write lets a caller manage accounts and roles, but never hand
// out more than the caller holds: every privilege a caller grants (by
// creating or changing a role, binding a role, or creating a user with
// roles) must be covered by one of the caller's own effective privileges,
// and acting on another account in a way that could take it over (setting
// its password, disabling or enabling it, changing or deleting it, ending
// its sessions) needs the caller to cover everything that account holds.
// Administrators (holders of system:*) can therefore only be managed by
// other holders of system:*. The same rule applies to tokens issued for a
// service account with system:tokens:write.
//
// Coverage is checked privilege by privilege (see covers): repo:team-*:read
// covers repo:team-a:read, and repo:*:admin covers every repository action,
// but two narrower privileges together never cover a wider one.

// errExceedsCaller is wrapped by the denials of this guard.
var errExceedsCaller = fmt.Errorf("it would exceed your own privileges: %w", core.ErrForbidden)

// superPrivilege is system:*, the administrator privilege.
var superPrivilege = core.Privilege{Domain: core.DomainSystem, Pattern: "*", Action: core.ActionAny}

// uncovered returns the privileges of want that no privilege of have covers.
func uncovered(have, want []core.Privilege) []core.Privilege {
	var out []core.Privilege
	for _, w := range want {
		if !slices.ContainsFunc(have, func(h core.Privilege) bool { return covers(h, w) }) && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// callerPrivileges are the effective privileges of the context principal
// (roles narrowed by token scopes).
func (s *Service) callerPrivileges(ctx context.Context) ([]core.Privilege, error) {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() {
		return nil, nil
	}
	return s.authz.Privileges(ctx, p)
}

// roleNamesPrivileges loads the privileges the named roles grant.
func (s *Service) roleNamesPrivileges(ctx context.Context, roles []string) ([]core.Privilege, error) {
	if len(roles) == 0 {
		return nil, nil
	}
	privs, err := s.rolePrivileges(ctx, roles)
	if err != nil {
		return nil, fmt.Errorf("load role privileges: %w", err)
	}
	return privs, nil
}

// requireCovers fails with ErrForbidden, and audits the denial as
// auditAction on target, unless the caller covers every privilege of want.
// what names the thing being granted or touched, for the error message.
func (s *Service) requireCovers(ctx context.Context, auditAction, target, what string, want []core.Privilege) error {
	if len(want) == 0 {
		return nil
	}
	have, err := s.callerPrivileges(ctx)
	if err != nil {
		return err
	}
	missing := uncovered(have, want)
	if len(missing) == 0 {
		return nil
	}
	reason := "exceeds_caller_privileges"
	msg := fmt.Sprintf("you cannot manage %s: it holds %s, which you do not", what, missing[0])
	if slices.Contains(missing, superPrivilege) {
		reason = "administrator"
		msg = fmt.Sprintf("only holders of %s can manage %s", superPrivilege, what)
	}
	s.record(ctx, core.AuditEvent{
		Action: auditAction, Target: target, Outcome: core.OutcomeDenied,
		Details: map[string]any{"reason": reason, "missing": privilegeStrings(missing)},
	})
	return fmt.Errorf("%s; %w", msg, errExceedsCaller)
}

// requireCoversRoles: the caller covers every privilege the named roles grant.
func (s *Service) requireCoversRoles(ctx context.Context, auditAction, target string, roles []string) error {
	want, err := s.roleNamesPrivileges(ctx, roles)
	if err != nil {
		return err
	}
	what := "these roles"
	if len(roles) == 1 {
		what = "role " + roles[0]
	}
	return s.requireCovers(ctx, auditAction, target, what, want)
}

// requireCoversUser: the caller covers everything account u holds. Acting on
// yourself needs no coverage.
func (s *Service) requireCoversUser(ctx context.Context, auditAction string, u *User) error {
	if u.ID == core.PrincipalFrom(ctx).ID {
		return nil
	}
	want, err := s.roleNamesPrivileges(ctx, u.Roles)
	if err != nil {
		return err
	}
	return s.requireCovers(ctx, auditAction, "user:"+u.Username, fmt.Sprintf("%s %q", accountNoun(u), u.Username), want)
}

func accountNoun(u *User) string {
	if u.Kind == KindServiceAccount {
		return "service account"
	}
	return "user"
}
