package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// User kinds and sources.
const (
	KindUser           = "user"
	KindServiceAccount = "service_account"
	SourceLocal        = "local"
	SourceToken        = "token"
)

// User is a member or service account.
type User struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	Kind         string     `json:"kind"`
	DisplayName  string     `json:"display_name"`
	Email        string     `json:"email,omitempty"`
	Description  string     `json:"description"`
	Source       string     `json:"source"`
	Disabled     bool       `json:"disabled"`
	LockedUntil  *time.Time `json:"locked_until,omitempty"`
	LastActiveAt *time.Time `json:"last_active_at,omitempty"`
	Roles        []string   `json:"roles"`
	TokenCount   int        `json:"token_count"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	// PasswordSet reports whether the user has a local password.
	PasswordSet bool `json:"password_set"`
	// MustChangePassword: the local user has to set a new password before
	// anything else (first sign-in, one-time password, administrator request).
	MustChangePassword bool `json:"must_change_password"`

	passwordHash string
	failedLogins int
}

// userCols selects a User from "users u"; scan with scanUser.
const userCols = `u.id::text, u.username, u.kind, coalesce(u.email, ''), u.display_name, u.source,
	u.disabled, u.locked_until, coalesce(u.password_hash, ''), u.failed_logins, u.last_active_at,
	u.created_at, u.updated_at, u.description,
	coalesce((SELECT array_agg(r.name ORDER BY r.name) FROM role_bindings b JOIN roles r ON r.id = b.role_id
	          WHERE b.user_id = u.id AND b.subject_kind = 'user'), '{}'),
	(SELECT count(*) FROM tokens t WHERE t.user_id = u.id AND t.kind = 'personal'
	   AND t.revoked_at IS NULL AND t.expires_at > now()),
	u.must_change_password`

func scanUser(row pgx.Row, extra ...any) (*User, error) {
	u := &User{}
	dest := append([]any{&u.ID, &u.Username, &u.Kind, &u.Email, &u.DisplayName, &u.Source,
		&u.Disabled, &u.LockedUntil, &u.passwordHash, &u.failedLogins, &u.LastActiveAt,
		&u.CreatedAt, &u.UpdatedAt, &u.Description, &u.Roles, &u.TokenCount, &u.MustChangePassword}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	u.PasswordSet = u.passwordHash != ""
	return u, nil
}

// principal is the core.Principal for u. A password sign-in (any source
// but a token) of a user who must change their password is restricted.
func (u *User) principal(source string) *core.Principal {
	kind := core.PrincipalUser
	if u.Kind == KindServiceAccount {
		kind = core.PrincipalServiceAccount
	}
	return &core.Principal{
		ID:          u.ID,
		Name:        u.Username,
		Kind:        kind,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		Source:      source,
		Roles:       append([]string{}, u.Roles...),

		PasswordChangeRequired: u.MustChangePassword && source != SourceToken,
	}
}

func (u *User) lockedAt(now time.Time) bool {
	return u.LockedUntil != nil && u.LockedUntil.After(now)
}

func (s *Service) userByName(ctx context.Context, q querier, username string) (*User, error) {
	u, err := scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM users u WHERE u.username = $1`, username))
	return u, dbErr(err, fmt.Sprintf("user %q", username))
}

func (s *Service) userByID(ctx context.Context, q querier, id string) (*User, error) {
	if !validUUID(id) {
		return nil, fmt.Errorf("user %q: %w", id, core.ErrNotFound)
	}
	u, err := scanUser(q.QueryRow(ctx, `SELECT `+userCols+` FROM users u WHERE u.id = $1`, id))
	return u, dbErr(err, fmt.Sprintf("user %q", id))
}

// rolePrivileges implements RolePrivileges from the roles/privileges tables.
func (s *Service) rolePrivileges(ctx context.Context, roles []string) ([]core.Privilege, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT p.privilege FROM roles r JOIN privileges p ON p.role_id = r.id
		WHERE r.name = ANY($1) ORDER BY 1`, roles)
	if err != nil {
		return nil, err
	}
	strs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]core.Privilege, 0, len(strs))
	for _, str := range strs {
		p, err := core.ParsePrivilege(str)
		if err != nil {
			s.log.Warn("auth: ignoring unparsable stored privilege", "privilege", str, "error", err)
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// querier is satisfied by *pgxpool.Pool and pgx.Tx.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

func parsePrivileges(strs []string) []core.Privilege {
	out := make([]core.Privilege, 0, len(strs))
	for _, str := range strs {
		if p, err := core.ParsePrivilege(str); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func privilegeStrings(ps []core.Privilege) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}
