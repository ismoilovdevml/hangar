package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// SystemActor is the audit actor for actions Hangar takes on its own.
const SystemActor = "system"

// bootstrapLock serializes first-administrator creation across nodes.
const bootstrapLock = `SELECT pg_advisory_xact_lock(hashtext('hangar.bootstrap_admin'))`

// EnsureBootstrapAdmin makes sure an administrator exists, for first runs.
// When no enabled user holds the Admin role it creates the local user
// username with password and the Admin role; if that local user already
// exists it is enabled, unlocked, given the password and the Admin role. With
// requireChange the user must set a new password at the first sign-in. When
// an enabled administrator exists nothing changes and created is false. The
// password must satisfy the password policy. Safe to run concurrently on
// several nodes.
func (s *Service) EnsureBootstrapAdmin(ctx context.Context, username, password string, requireChange bool) (created bool, err error) {
	if err := ValidateUsername(username); err != nil {
		return false, err
	}
	if err := s.cfg.Password.Check(username, password); err != nil {
		return false, err
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return false, err
	}
	var userID string
	var promoted bool
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, bootstrapLock); err != nil {
			return dbErr(err, "bootstrap lock")
		}
		var admins int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM users u
			JOIN role_bindings b ON b.user_id = u.id AND b.subject_kind = 'user'
			JOIN roles r ON r.id = b.role_id
			WHERE r.name = $1 AND NOT u.disabled`, AdminRole).Scan(&admins); err != nil {
			return dbErr(err, "count administrators")
		}
		if admins > 0 {
			return nil
		}
		existing, err := s.userByName(ctx, tx, username)
		switch {
		case errors.Is(err, core.ErrNotFound):
			if err := tx.QueryRow(ctx, `
				INSERT INTO users (username, kind, display_name, password_hash, password_changed_at, source, must_change_password)
				VALUES ($1, 'user', $1, $2, now(), 'local', $3) RETURNING id::text`, username, hash, requireChange).Scan(&userID); err != nil {
				return dbErr(err, "create administrator")
			}
		case err != nil:
			return err
		case existing.Kind != KindUser || existing.Source != SourceLocal:
			return fmt.Errorf("user %q exists and is not a local user: %w", username, core.ErrAlreadyExists)
		default:
			userID, promoted = existing.ID, true
			if _, err := tx.Exec(ctx, `
				UPDATE users SET password_hash = $2, password_changed_at = now(), disabled = false,
					failed_logins = 0, failed_window_start = NULL, locked_until = NULL,
					must_change_password = $3, updated_at = now()
				WHERE id = $1`, userID, hash, requireChange); err != nil {
				return dbErr(err, "promote administrator")
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO role_bindings (role_id, subject_kind, user_id)
			SELECT id, 'user', $1 FROM roles WHERE name = $2
			ON CONFLICT DO NOTHING`, userID, AdminRole)
		return dbErr(err, "bind administrator role")
	})
	if err != nil || userID == "" {
		return false, err
	}
	s.credentialsChanged()
	ctx = core.WithPrincipal(ctx, &core.Principal{Name: SystemActor, Kind: core.PrincipalServiceAccount})
	s.record(ctx, core.AuditEvent{
		Actor: SystemActor, Action: AuditUserBootstrap, Target: "user:" + username,
		Details: map[string]any{"id": userID, "existing_user": promoted, "require_change": requireChange},
	})
	return true, nil
}

// InitialAdminUsername is the user EnsureInitialAdmin creates.
const InitialAdminUsername = "admin"

// InitialAdmin is the administrator EnsureInitialAdmin created.
type InitialAdmin struct {
	Username string
	// Password is the generated one-time password; empty when it was read
	// from the initial password file.
	Password string
	// FromFile: the password was taken from the initial password file.
	FromFile bool
	// File is the initial password file holding the password, or "".
	File string
}

// EnsureInitialAdmin creates the first administrator of an empty
// installation. When the users table is empty it creates the local user
// "admin" with the Admin role and a one-time password that must be replaced
// at the first sign-in:
//
//   - if the initial password file (Options.InitialPasswordFile) exists and
//     holds a password, that password is used (it must satisfy the policy);
//   - otherwise a strong password is generated and, when a file is
//     configured, written to it with mode 0600 before the user is committed.
//
// The file is deleted once the password is replaced (ChangePassword). With
// any user present nothing happens and the result is nil. Safe to run
// concurrently on several nodes: exactly one creates the user.
func (s *Service) EnsureInitialAdmin(ctx context.Context) (*InitialAdmin, error) {
	var out *InitialAdmin
	var userID string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, bootstrapLock); err != nil {
			return dbErr(err, "bootstrap lock")
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users)`).Scan(&exists); err != nil {
			return dbErr(err, "count users")
		}
		if exists {
			return nil
		}
		a := &InitialAdmin{Username: InitialAdminUsername, File: s.initialPasswordFile}
		password := ""
		if a.File != "" {
			stored, ok, err := readInitialPassword(a.File)
			if err != nil {
				return err
			}
			if ok {
				if err := s.cfg.Password.Check(a.Username, stored); err != nil {
					return fmt.Errorf("initial password file %s: %w", a.File, err)
				}
				password, a.FromFile = stored, true
			}
		}
		if password == "" {
			generated, err := GeneratePassword(s.cfg.Password, a.Username)
			if err != nil {
				return err
			}
			password, a.Password = generated, generated
		}
		hash, err := s.hasher.Hash(ctx, password)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (username, kind, display_name, password_hash, password_changed_at, source, must_change_password)
			VALUES ($1, 'user', $1, $2, now(), 'local', true) RETURNING id::text`, a.Username, hash).Scan(&userID); err != nil {
			return dbErr(err, "create administrator")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO role_bindings (role_id, subject_kind, user_id)
			SELECT id, 'user', $1 FROM roles WHERE name = $2`, userID, AdminRole); err != nil {
			return dbErr(err, "bind administrator role")
		}
		// Written before the commit: if the commit fails, the next start
		// finds the file and uses the same password.
		if a.File != "" && !a.FromFile {
			if err := writeInitialPassword(a.File, password); err != nil {
				return err
			}
		}
		out = a
		return nil
	})
	if err != nil || out == nil {
		return nil, err
	}
	s.credentialsChanged()
	ctx = core.WithPrincipal(ctx, &core.Principal{Name: SystemActor, Kind: core.PrincipalServiceAccount})
	source := "generated"
	if out.FromFile {
		source = "file"
	}
	s.record(ctx, core.AuditEvent{
		Actor: SystemActor, Action: AuditUserBootstrap, Target: "user:" + out.Username,
		Details: map[string]any{"id": userID, "existing_user": false, "require_change": true, "password_source": source},
	})
	return out, nil
}
