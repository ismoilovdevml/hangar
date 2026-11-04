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

// EnsureBootstrapAdmin makes sure an administrator exists, for first runs.
// When no enabled user holds the Admin role it creates the local user
// username with password and the Admin role; if that local user already
// exists it is enabled, unlocked, given the password and the Admin role. When
// an enabled administrator exists nothing changes and created is false. The
// password must satisfy the password policy. Safe to run concurrently on
// several nodes.
func (s *Service) EnsureBootstrapAdmin(ctx context.Context, username, password string) (created bool, err error) {
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
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('hangar.bootstrap_admin'))`); err != nil {
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
				INSERT INTO users (username, kind, display_name, password_hash, password_changed_at, source)
				VALUES ($1, 'user', $1, $2, now(), 'local') RETURNING id::text`, username, hash).Scan(&userID); err != nil {
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
					failed_logins = 0, failed_window_start = NULL, locked_until = NULL, updated_at = now()
				WHERE id = $1`, userID, hash); err != nil {
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
		Details: map[string]any{"id": userID, "existing_user": promoted},
	})
	return true, nil
}
