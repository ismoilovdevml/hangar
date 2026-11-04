package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// errWrongPassword is returned by ChangePassword when the current password
// does not match.
var errWrongPassword = fmt.Errorf("the current password is incorrect: %w", core.ErrInvalid)

// endSessions deletes the user's browser sessions except keep (a session id,
// or "" for none) and revokes their short-lived session tokens (registry
// bearer tokens, CLI logins). Personal access tokens are not touched. It
// returns the number of browser sessions ended.
func endSessions(ctx context.Context, tx pgx.Tx, userID, keep string) (int64, error) {
	var keepID *string
	if validUUID(keep) {
		keepID = &keep
	}
	tag, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND ($2::uuid IS NULL OR id <> $2)`, userID, keepID)
	if err != nil {
		return 0, dbErr(err, "end sessions")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE tokens SET revoked_at = now()
		WHERE user_id = $1 AND kind = 'session' AND revoked_at IS NULL`, userID); err != nil {
		return 0, dbErr(err, "revoke session tokens")
	}
	return tag.RowsAffected(), nil
}

// SetUserDisabled disables or re-enables an account (system:users:write).
// Disabling ends the user's sessions; their tokens stop working while the
// account is disabled. You cannot disable yourself or the last enabled
// administrator.
func (s *Service) SetUserDisabled(ctx context.Context, id string, disabled bool) (*User, error) {
	action := AuditUserEnable
	if disabled {
		action = AuditUserDisable
	}
	if err := s.require(ctx, action, core.ActionWrite, core.SystemResource(core.SystemUsers)); err != nil {
		return nil, err
	}
	if disabled && id == core.PrincipalFrom(ctx).ID {
		return nil, fmt.Errorf("you cannot disable your own account: %w", core.ErrInvalid)
	}
	var old *User
	var ended int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if old, err = s.lockUser(ctx, tx, id); err != nil {
			return err
		}
		if old.Disabled == disabled {
			return nil
		}
		if disabled && slices.Contains(old.Roles, AdminRole) {
			if err := ensureOtherAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET disabled = $2, updated_at = now() WHERE id = $1`, id, disabled); err != nil {
			return dbErr(err, "update user")
		}
		if disabled {
			ended, err = endSessions(ctx, tx, id, "")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if old.Disabled != disabled {
		s.credentialsChanged()
		s.record(ctx, core.AuditEvent{
			Action: action, Target: "user:" + old.Username,
			Details: map[string]any{"id": id, "kind": old.Kind, "sessions_ended": ended},
		})
	}
	return s.userByID(ctx, s.pool, id)
}

// ResetPassword sets another local user's password (system:users:write),
// clears any lockout and ends all their sessions. Administrators change their
// own password with ChangePassword, which needs the current one.
func (s *Service) ResetPassword(ctx context.Context, id, password string) error {
	if err := s.require(ctx, AuditUserPassword, core.ActionWrite, core.SystemResource(core.SystemUsers)); err != nil {
		return err
	}
	if id == core.PrincipalFrom(ctx).ID {
		return fmt.Errorf("change your own password with PUT /me/password (it needs the current password): %w", core.ErrInvalid)
	}
	u, err := s.userByID(ctx, s.pool, id)
	if err != nil {
		return err
	}
	if u.Kind != KindUser || u.Source != SourceLocal {
		return fmt.Errorf("user %q has no local password (%s %s): %w", u.Username, u.Source, u.Kind, core.ErrInvalid)
	}
	if err := s.cfg.Password.Check(u.Username, password); err != nil {
		return err
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return err
	}
	var ended int64
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE users SET password_hash = $2, password_changed_at = now(), failed_logins = 0,
				failed_window_start = NULL, locked_until = NULL, updated_at = now()
			WHERE id = $1`, id, hash); err != nil {
			return dbErr(err, "reset password")
		}
		ended, err = endSessions(ctx, tx, id, "")
		return err
	})
	if err != nil {
		return err
	}
	s.credentialsChanged()
	s.record(ctx, core.AuditEvent{
		Action: AuditUserPassword, Target: "user:" + u.Username,
		Details: map[string]any{"id": id, "reset_by_admin": true, "sessions_ended": ended},
	})
	return nil
}

// ChangePassword changes the caller's own local password. The current
// password is required; wrong guesses count towards the account lockout.
// Every other session of the caller ends (keepSession, the session making
// the request, stays signed in) and their short-lived session tokens are
// revoked. Personal access tokens keep working.
func (s *Service) ChangePassword(ctx context.Context, current, next, keepSession string) error {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() || !validUUID(p.ID) {
		return fmt.Errorf("change password: %w", core.ErrUnauthenticated)
	}
	if p.TokenID != "" {
		s.record(ctx, core.AuditEvent{Action: AuditUserPassword, Target: "user:" + p.Name, Outcome: core.OutcomeDenied,
			Details: map[string]any{"reason": "token"}})
		return fmt.Errorf("sign in with your password to change it; tokens cannot: %w", core.ErrForbidden)
	}
	u, err := s.userByID(ctx, s.pool, p.ID)
	if err != nil {
		return err
	}
	if u.Kind != KindUser || u.Source != SourceLocal || u.passwordHash == "" {
		return fmt.Errorf("your account has no local password; it is managed by %s: %w", u.Source, core.ErrInvalid)
	}
	now := s.now()
	if u.lockedAt(now) {
		return &LockedError{Until: *u.LockedUntil}
	}
	if len(current) > 4096 {
		return errWrongPassword
	}
	ok, _, err := s.hasher.Verify(ctx, u.passwordHash, current)
	if err != nil && !errors.Is(err, errBadHash) {
		return fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		attempts, lockedUntil, err := s.recordFailure(ctx, u.ID, now)
		if err != nil {
			return err
		}
		details := map[string]any{"reason": "bad_password", "attempts": attempts}
		if lockedUntil != nil {
			details["locked_until"] = lockedUntil.UTC()
		}
		s.record(ctx, core.AuditEvent{Action: AuditUserPassword, Target: "user:" + u.Username, Outcome: core.OutcomeDenied, Details: details})
		if lockedUntil != nil {
			s.basic.clear()
			return &LockedError{Until: *lockedUntil}
		}
		return errWrongPassword
	}
	if current == next {
		if u.MustChangePassword {
			return fmt.Errorf("choose a new password; the one you were given cannot be kept: %w", core.ErrInvalid)
		}
		return fmt.Errorf("the new password must differ from the current one: %w", core.ErrInvalid)
	}
	if err := s.cfg.Password.Check(u.Username, next); err != nil {
		return err
	}
	hash, err := s.hasher.Hash(ctx, next)
	if err != nil {
		return err
	}
	var ended int64
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE users SET password_hash = $2, password_changed_at = now(), failed_logins = 0,
				failed_window_start = NULL, locked_until = NULL, must_change_password = false, updated_at = now()
			WHERE id = $1 AND password_hash = $3`, u.ID, hash, u.passwordHash)
		if err != nil {
			return dbErr(err, "change password")
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("the password was changed concurrently; try again: %w", core.ErrAlreadyExists)
		}
		ended, err = endSessions(ctx, tx, u.ID, keepSession)
		return err
	})
	if err != nil {
		return err
	}
	s.credentialsChanged()
	if u.MustChangePassword {
		s.record(ctx, core.AuditEvent{
			Action: AuditUserPasswordChanged, Target: "user:" + u.Username,
			Details: map[string]any{"id": u.ID, "self": true, "reason": "first_login", "other_sessions_ended": ended},
		})
	} else {
		s.record(ctx, core.AuditEvent{
			Action: AuditUserPassword, Target: "user:" + u.Username,
			Details: map[string]any{"id": u.ID, "self": true, "other_sessions_ended": ended},
		})
	}
	s.discardInitialPassword(ctx, func(stored string) bool { return equalSecret(stored, current) })
	return nil
}

// ProfileInput is what users may change about themselves. Nil fields are
// left unchanged.
type ProfileInput struct {
	DisplayName *string `json:"display_name"`
	Email       *string `json:"email"`
}

// UpdateProfile changes the caller's display name and email. External
// identities (OIDC, LDAP, SAML) are managed by their provider. Scoped tokens
// cannot change the profile.
func (s *Service) UpdateProfile(ctx context.Context, in ProfileInput) (*User, error) {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() || !validUUID(p.ID) {
		return nil, fmt.Errorf("update profile: %w", core.ErrUnauthenticated)
	}
	if p.Scopes != nil {
		return nil, fmt.Errorf("a scoped token cannot change the profile: %w", core.ErrForbidden)
	}
	if err := validateProfile(in.DisplayName, in.Email); err != nil {
		return nil, err
	}
	var old *User
	var changed []string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if old, err = s.lockUser(ctx, tx, p.ID); err != nil {
			return err
		}
		if old.Source != SourceLocal {
			return fmt.Errorf("your profile is managed by %s: %w", old.Source, core.ErrInvalid)
		}
		if in.DisplayName != nil && *in.DisplayName != old.DisplayName {
			changed = append(changed, "display_name")
		}
		if in.Email != nil && *in.Email != old.Email {
			changed = append(changed, "email")
		}
		if len(changed) == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `
			UPDATE users SET display_name = coalesce($2, display_name),
				email = CASE WHEN $3::boolean THEN $4 ELSE email END, updated_at = now()
			WHERE id = $1`, p.ID, in.DisplayName, in.Email != nil, nullIfEmpty(in.Email))
		return dbErr(err, "update profile")
	})
	if err != nil {
		return nil, err
	}
	if len(changed) > 0 {
		s.record(ctx, core.AuditEvent{
			Action: AuditUserUpdate, Target: "user:" + old.Username,
			Details: map[string]any{"id": p.ID, "changed": changed, "self": true},
		})
	}
	return s.userByID(ctx, s.pool, p.ID)
}
