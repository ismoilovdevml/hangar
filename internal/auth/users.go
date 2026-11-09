package auth

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// AdminRole is the built-in role with every privilege.
const AdminRole = "Admin"

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)

// reservedNames are used as audit actors and cannot be usernames.
var reservedNames = []string{core.AnonymousName, "system"}

// ValidateUsername enforces the username rule.
func ValidateUsername(name string) error {
	if !usernameRE.MatchString(name) {
		return fmt.Errorf("username %q must match %s: %w", name, usernameRE.String(), core.ErrInvalid)
	}
	if slices.Contains(reservedNames, strings.ToLower(name)) {
		return fmt.Errorf("username %q is reserved: %w", name, core.ErrInvalid)
	}
	return nil
}

// UserInput creates or updates a user (schema MemberInput). On update, nil
// fields are left unchanged.
type UserInput struct {
	Username    string   `json:"username"`
	Kind        string   `json:"kind"`
	DisplayName *string  `json:"display_name"`
	Email       *string  `json:"email"`
	Description *string  `json:"description"`
	Password    *string  `json:"password"`
	Roles       []string `json:"roles"`
	Disabled    *bool    `json:"disabled"`
}

func (in *UserInput) validate(policy PasswordPolicy) error {
	if err := ValidateUsername(in.Username); err != nil {
		return err
	}
	if in.Kind != KindUser && in.Kind != KindServiceAccount {
		return fmt.Errorf("kind %q: want user or service_account: %w", in.Kind, core.ErrInvalid)
	}
	if err := validateProfile(in.DisplayName, in.Email); err != nil {
		return err
	}
	if in.Description != nil && utf8.RuneCountInString(*in.Description) > 1000 {
		return fmt.Errorf("description is too long (at most 1000 characters): %w", core.ErrInvalid)
	}
	if in.Password != nil {
		if in.Kind == KindServiceAccount {
			return fmt.Errorf("service accounts authenticate with tokens and have no password: %w", core.ErrInvalid)
		}
		if err := policy.Check(in.Username, *in.Password); err != nil {
			return err
		}
	}
	return nil
}

// validateProfile checks the fields a user may also change about themself.
func validateProfile(displayName, email *string) error {
	if displayName != nil && utf8.RuneCountInString(*displayName) > 200 {
		return fmt.Errorf("display_name is too long (at most 200 characters): %w", core.ErrInvalid)
	}
	if email != nil && *email != "" {
		e := *email
		if len(e) > 254 || strings.ContainsAny(e, " \t\r\n") || strings.Count(e, "@") != 1 || strings.HasPrefix(e, "@") || strings.HasSuffix(e, "@") {
			return fmt.Errorf("email %q is not valid: %w", e, core.ErrInvalid)
		}
	}
	return nil
}

// User status filters for UserQuery.Status.
const (
	StatusActive   = "active"   // enabled and not locked
	StatusDisabled = "disabled" // disabled by an administrator
	StatusLocked   = "locked"   // locked after failed sign-ins
)

// UserQuery filters ListUsers.
type UserQuery struct {
	Q      string // substring of username, display name or email
	Kind   string // user, service_account or empty
	Role   string // holds this role directly; empty for any
	Status string // StatusActive, StatusDisabled, StatusLocked or empty
	Source string // local, oidc, ldap, saml or empty
	Limit  int
	Cursor string
}

func (q UserQuery) validate() error {
	if q.Kind != "" && q.Kind != KindUser && q.Kind != KindServiceAccount {
		return fmt.Errorf("kind %q: want user or service_account: %w", q.Kind, core.ErrInvalid)
	}
	switch q.Status {
	case "", StatusActive, StatusDisabled, StatusLocked:
	default:
		return fmt.Errorf("status %q: want active, disabled or locked: %w", q.Status, core.ErrInvalid)
	}
	switch q.Source {
	case "", SourceLocal, "oidc", "ldap", "saml":
	default:
		return fmt.Errorf("source %q: want local, oidc, ldap or saml: %w", q.Source, core.ErrInvalid)
	}
	if len(q.Q) > 256 || len(q.Role) > 64 {
		return fmt.Errorf("filter is too long: %w", core.ErrInvalid)
	}
	return nil
}

// CreateUser creates a local user or service account (system:users:write).
func (s *Service) CreateUser(ctx context.Context, in UserInput) (*User, error) {
	if err := s.require(ctx, AuditUserCreate, core.ActionWrite, core.SystemResource(core.SystemUsers)); err != nil {
		return nil, err
	}
	if err := in.validate(s.cfg.Password); err != nil {
		return nil, err
	}
	if err := s.requireCoversRoles(ctx, AuditUserCreate, "user:"+in.Username, in.Roles); err != nil {
		return nil, err
	}
	var hash *string
	if in.Password != nil {
		h, err := s.hasher.Hash(ctx, *in.Password)
		if err != nil {
			return nil, err
		}
		hash = &h
	}
	var id string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO users (username, kind, email, display_name, description, password_hash, password_changed_at, disabled, source)
			VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $6::text IS NULL THEN NULL ELSE now() END, $7, 'local')
			RETURNING id::text`,
			in.Username, in.Kind, nullIfEmpty(in.Email), deref(in.DisplayName), deref(in.Description), hash, derefBool(in.Disabled)).Scan(&id)
		if err != nil {
			return dbErr(err, fmt.Sprintf("create user %q", in.Username))
		}
		return setUserRoles(ctx, tx, id, in.Roles)
	})
	if err != nil {
		return nil, err
	}
	s.record(ctx, core.AuditEvent{
		Action: AuditUserCreate, Target: "user:" + in.Username,
		Details: map[string]any{"id": id, "kind": in.Kind, "roles": nonNil(in.Roles), "password_set": hash != nil},
	})
	return s.userByID(ctx, s.pool, id)
}

// GetUser returns a user by id (system:users:read, or the caller themself).
func (s *Service) GetUser(ctx context.Context, id string) (*User, error) {
	if id != core.PrincipalFrom(ctx).ID {
		if err := s.require(ctx, "user.read", core.ActionRead, core.SystemResource(core.SystemUsers)); err != nil {
			return nil, err
		}
	}
	return s.userByID(ctx, s.pool, id)
}

// GetUserByName returns a user without authorization (internal callers).
func (s *Service) GetUserByName(ctx context.Context, username string) (*User, error) {
	return s.userByName(ctx, s.pool, username)
}

// ListUsers lists users ordered by username (system:users:read).
func (s *Service) ListUsers(ctx context.Context, q UserQuery) (core.Page[*User], error) {
	var page core.Page[*User]
	if err := s.require(ctx, "user.list", core.ActionRead, core.SystemResource(core.SystemUsers)); err != nil {
		return page, err
	}
	if err := q.validate(); err != nil {
		return page, err
	}
	after, err := decodeStringCursor(q.Cursor)
	if err != nil {
		return page, err
	}
	limit := pageLimit(q.Limit)
	var pattern *string
	if q.Q != "" {
		p := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q.Q) + "%"
		pattern = &p
	}
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users u
		WHERE ($1::text IS NULL OR u.username ILIKE $1 OR u.display_name ILIKE $1 OR u.email ILIKE $1)
		  AND ($2 = '' OR u.kind = $2)
		  AND ($3::text IS NULL OR u.username > $3)
		  AND ($5 = '' OR EXISTS (SELECT 1 FROM role_bindings b JOIN roles r ON r.id = b.role_id
		                          WHERE b.user_id = u.id AND b.subject_kind = 'user' AND r.name = $5))
		  AND ($6 = '' OR u.source = $6)
		  AND ($7 = ''
		       OR ($7 = 'disabled' AND u.disabled)
		       OR ($7 = 'locked' AND NOT u.disabled AND u.locked_until > $8)
		       OR ($7 = 'active' AND NOT u.disabled AND (u.locked_until IS NULL OR u.locked_until <= $8)))
		ORDER BY u.username LIMIT $4`, pattern, q.Kind, after, limit+1, q.Role, q.Source, q.Status, s.now())
	if err != nil {
		return page, dbErr(err, "list users")
	}
	page.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (*User, error) { return scanUser(r) })
	if err != nil {
		return page, dbErr(err, "list users")
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = encodeStringCursor(page.Items[limit-1].Username)
	}
	return page, nil
}

// UpdateUser changes a user (system:users:write). Kind cannot change. Setting
// a password clears any lockout and makes the user choose a new one at the
// next sign-in; disabling or changing the password ends the user's sessions.
// The caller must cover the user's current privileges and any roles given.
func (s *Service) UpdateUser(ctx context.Context, id string, in UserInput) (*User, error) {
	if err := s.require(ctx, AuditUserUpdate, core.ActionWrite, core.SystemResource(core.SystemUsers)); err != nil {
		return nil, err
	}
	if err := in.validate(s.cfg.Password); err != nil {
		return nil, err
	}
	caller := core.PrincipalFrom(ctx)
	if id == caller.ID && in.Disabled != nil && *in.Disabled {
		return nil, fmt.Errorf("you cannot disable your own account: %w", core.ErrInvalid)
	}
	if id == caller.ID && in.Password != nil {
		return nil, fmt.Errorf("change your own password with PUT /me/password (it needs the current password): %w", core.ErrInvalid)
	}
	var hash *string
	if in.Password != nil {
		h, err := s.hasher.Hash(ctx, *in.Password)
		if err != nil {
			return nil, err
		}
		hash = &h
	}
	var changed []string
	var old *User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if old, err = s.lockUser(ctx, tx, id); err != nil {
			return err
		}
		if err := s.requireCoversUser(ctx, AuditUserUpdate, old); err != nil {
			return err
		}
		if in.Roles != nil {
			if err := s.requireCoversRoles(ctx, AuditUserUpdate, "user:"+old.Username, in.Roles); err != nil {
				return err
			}
		}
		if old.Kind != in.Kind {
			return fmt.Errorf("the kind of user %q cannot change: %w", old.Username, core.ErrInvalid)
		}
		if in.Password != nil && old.Source != SourceLocal {
			return fmt.Errorf("user %q signs in with %s and has no local password: %w", old.Username, old.Source, core.ErrInvalid)
		}
		disabling := in.Disabled != nil && *in.Disabled && !old.Disabled
		dropsAdmin := in.Roles != nil && slices.Contains(old.Roles, AdminRole) && !slices.Contains(in.Roles, AdminRole)
		if slices.Contains(old.Roles, AdminRole) && !old.Disabled && (disabling || dropsAdmin) {
			if err := ensureOtherAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		if in.Username != old.Username {
			changed = append(changed, "username")
		}
		if in.DisplayName != nil && *in.DisplayName != old.DisplayName {
			changed = append(changed, "display_name")
		}
		if in.Email != nil && *in.Email != old.Email {
			changed = append(changed, "email")
		}
		if in.Description != nil && *in.Description != old.Description {
			changed = append(changed, "description")
		}
		if in.Disabled != nil && *in.Disabled != old.Disabled {
			changed = append(changed, "disabled")
		}
		if hash != nil {
			changed = append(changed, "password")
		}
		_, err = tx.Exec(ctx, `
			UPDATE users SET
				username = $2,
				display_name = coalesce($3, display_name),
				email = CASE WHEN $4::boolean THEN $5 ELSE email END,
				description = coalesce($8, description),
				disabled = coalesce($6, disabled),
				password_hash = coalesce($7, password_hash),
				password_changed_at = CASE WHEN $7::text IS NULL THEN password_changed_at ELSE now() END,
				must_change_password = CASE WHEN $7::text IS NULL THEN must_change_password ELSE true END,
				failed_logins = CASE WHEN $7::text IS NULL THEN failed_logins ELSE 0 END,
				failed_window_start = CASE WHEN $7::text IS NULL THEN failed_window_start END,
				locked_until = CASE WHEN $7::text IS NULL THEN locked_until END,
				updated_at = now()
			WHERE id = $1`,
			id, in.Username, in.DisplayName, in.Email != nil, nullIfEmpty(in.Email), in.Disabled, hash, in.Description)
		if err != nil {
			return dbErr(err, fmt.Sprintf("update user %q", old.Username))
		}
		if in.Roles != nil {
			if !slices.Equal(slices.Sorted(slices.Values(in.Roles)), old.Roles) {
				changed = append(changed, "roles")
			}
			if err := setUserRoles(ctx, tx, id, in.Roles); err != nil {
				return err
			}
		}
		if hash != nil || slices.Contains(changed, "disabled") || slices.Contains(changed, "username") {
			if _, err := endSessions(ctx, tx, id, ""); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.credentialsChanged()
	s.record(ctx, core.AuditEvent{
		Action: AuditUserUpdate, Target: "user:" + in.Username,
		Details: map[string]any{"id": id, "changed": nonNil(changed), "previous_username": old.Username},
	})
	return s.userByID(ctx, s.pool, id)
}

// DeleteUser deletes a user with their tokens, sessions and role bindings
// (system:users:delete). The last enabled administrator and the caller cannot
// be deleted.
func (s *Service) DeleteUser(ctx context.Context, id string) error {
	if err := s.require(ctx, AuditUserDelete, core.ActionDelete, core.SystemResource(core.SystemUsers)); err != nil {
		return err
	}
	if id == core.PrincipalFrom(ctx).ID {
		return fmt.Errorf("you cannot delete your own account: %w", core.ErrInvalid)
	}
	var old *User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if old, err = s.lockUser(ctx, tx, id); err != nil {
			return err
		}
		if err := s.requireCoversUser(ctx, AuditUserDelete, old); err != nil {
			return err
		}
		if slices.Contains(old.Roles, AdminRole) && !old.Disabled {
			if err := ensureOtherAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
		return dbErr(err, "delete user")
	})
	if err != nil {
		return err
	}
	s.credentialsChanged()
	s.record(ctx, core.AuditEvent{
		Action: AuditUserDelete, Target: "user:" + old.Username, Details: map[string]any{"id": id, "kind": old.Kind},
	})
	return nil
}

// UnlockUser clears a lockout after failed sign-ins (system:users:write) and
// returns the user.
func (s *Service) UnlockUser(ctx context.Context, id string) (*User, error) {
	if err := s.require(ctx, AuditUserUnlock, core.ActionWrite, core.SystemResource(core.SystemUsers)); err != nil {
		return nil, err
	}
	u, err := s.userByID(ctx, s.pool, id)
	if err != nil {
		return nil, err
	}
	wasLocked := u.lockedAt(s.now())
	if _, err := s.pool.Exec(ctx, `
		UPDATE users SET failed_logins = 0, failed_window_start = NULL, locked_until = NULL, updated_at = now()
		WHERE id = $1`, id); err != nil {
		return nil, dbErr(err, "unlock user")
	}
	s.record(ctx, core.AuditEvent{Action: AuditUserUnlock, Target: "user:" + u.Username,
		Details: map[string]any{"id": id, "was_locked": wasLocked}})
	return s.userByID(ctx, s.pool, id)
}

func (s *Service) lockUser(ctx context.Context, tx pgx.Tx, id string) (*User, error) {
	if !validUUID(id) {
		return nil, fmt.Errorf("user %q: %w", id, core.ErrNotFound)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, id); err != nil {
		return nil, dbErr(err, "lock user")
	}
	return s.userByID(ctx, tx, id)
}

// ensureOtherAdmin fails unless an enabled Admin other than userID exists.
// Admin bindings are locked so two concurrent demotions cannot both pass.
func ensureOtherAdmin(ctx context.Context, tx pgx.Tx, userID string) error {
	var n int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM (
			SELECT u.id FROM users u
			JOIN role_bindings b ON b.user_id = u.id AND b.subject_kind = 'user'
			JOIN roles r ON r.id = b.role_id
			WHERE r.name = $1 AND NOT u.disabled AND u.id <> $2
			FOR UPDATE OF u
		) x`, AdminRole, userID).Scan(&n)
	if err != nil {
		return dbErr(err, "count administrators")
	}
	if n == 0 {
		return fmt.Errorf("at least one enabled user must keep the %s role: %w", AdminRole, core.ErrInvalid)
	}
	return nil
}

// setUserRoles replaces the user's direct role bindings with roles (nil = unchanged).
func setUserRoles(ctx context.Context, tx pgx.Tx, userID string, roles []string) error {
	if roles == nil {
		return nil
	}
	roles = slices.Compact(slices.Sorted(slices.Values(roles)))
	rows, err := tx.Query(ctx, `SELECT id::text, name FROM roles WHERE name = ANY($1)`, roles)
	if err != nil {
		return dbErr(err, "load roles")
	}
	ids := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		ids[name] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	roleIDs := make([]string, 0, len(roles))
	for _, r := range roles {
		id, ok := ids[r]
		if !ok {
			return fmt.Errorf("role %q does not exist: %w", r, core.ErrInvalid)
		}
		roleIDs = append(roleIDs, id)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM role_bindings WHERE user_id = $1 AND subject_kind = 'user' AND role_id <> ALL($2::uuid[])`,
		userID, roleIDs); err != nil {
		return dbErr(err, "update role bindings")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO role_bindings (role_id, subject_kind, user_id)
		SELECT r, 'user', $1 FROM unnest($2::uuid[]) AS r
		ON CONFLICT DO NOTHING`, userID, roleIDs); err != nil {
		return dbErr(err, "update role bindings")
	}
	return nil
}

func nullIfEmpty(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefBool(b *bool) bool { return b != nil && *b }

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
