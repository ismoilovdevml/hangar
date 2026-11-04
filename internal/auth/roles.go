package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ErrBuiltinRole is returned when changing or deleting a built-in role.
var ErrBuiltinRole = fmt.Errorf("built-in roles cannot be changed: %w", core.ErrAlreadyExists)

var roleNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)

// GroupMapping grants a role to an external group of an identity provider.
type GroupMapping struct {
	Provider string `json:"provider"`
	Group    string `json:"group"`
}

// RoleInput creates or replaces a role.
type RoleInput struct {
	Name          string           `json:"name"`
	Description   string           `json:"description"`
	Privileges    []core.Privilege `json:"privileges"`
	GroupMappings []GroupMapping   `json:"group_mappings"`
}

// Role is a named set of privileges.
type Role struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Description   string           `json:"description"`
	Privileges    []core.Privilege `json:"privileges"`
	GroupMappings []GroupMapping   `json:"group_mappings"`
	Builtin       bool             `json:"builtin"`
	MemberCount   int              `json:"member_count"`
}

func (in *RoleInput) validate() error {
	if !roleNameRE.MatchString(in.Name) {
		return fmt.Errorf("role name %q must match %s: %w", in.Name, roleNameRE.String(), core.ErrInvalid)
	}
	if len(in.Description) > 1000 {
		return fmt.Errorf("role description is too long: %w", core.ErrInvalid)
	}
	for _, g := range in.GroupMappings {
		if g.Provider == "" || g.Group == "" || len(g.Group) > 512 {
			return fmt.Errorf("group mapping needs a provider and a group: %w", core.ErrInvalid)
		}
	}
	return nil
}

// ListRoles lists every role (system:users:read).
func (s *Service) ListRoles(ctx context.Context) ([]*Role, error) {
	if err := s.require(ctx, "role.list", core.ActionRead, core.SystemResource(core.SystemUsers)); err != nil {
		return nil, err
	}
	return s.roles(ctx, s.pool, "")
}

// GetRole returns one role by name (system:users:read).
func (s *Service) GetRole(ctx context.Context, name string) (*Role, error) {
	if err := s.require(ctx, "role.read", core.ActionRead, core.SystemResource(core.SystemUsers)); err != nil {
		return nil, err
	}
	return s.role(ctx, s.pool, name)
}

func (s *Service) role(ctx context.Context, q querier, name string) (*Role, error) {
	rs, err := s.roles(ctx, q, name)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, fmt.Errorf("role %q: %w", name, core.ErrNotFound)
	}
	return rs[0], nil
}

func (s *Service) roles(ctx context.Context, q querier, name string) ([]*Role, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id::text, r.name, r.description, r.builtin,
			coalesce((SELECT array_agg(p.privilege ORDER BY p.privilege) FROM privileges p WHERE p.role_id = r.id), '{}'),
			coalesce((SELECT array_agg(ip.name ORDER BY ip.name, b.group_name) FROM role_bindings b JOIN identity_providers ip ON ip.id = b.provider_id
			          WHERE b.role_id = r.id AND b.subject_kind = 'group'), '{}'),
			coalesce((SELECT array_agg(b.group_name ORDER BY ip.name, b.group_name) FROM role_bindings b JOIN identity_providers ip ON ip.id = b.provider_id
			          WHERE b.role_id = r.id AND b.subject_kind = 'group'), '{}'),
			(SELECT count(*) FROM role_bindings b WHERE b.role_id = r.id AND b.subject_kind = 'user')
		FROM roles r
		WHERE $1 = '' OR r.name = $1
		ORDER BY r.builtin DESC, r.name`, name)
	if err != nil {
		return nil, dbErr(err, "list roles")
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*Role, error) {
		r := &Role{}
		var privs, providers, groups []string
		if err := row.Scan(&r.ID, &r.Name, &r.Description, &r.Builtin, &privs, &providers, &groups, &r.MemberCount); err != nil {
			return nil, err
		}
		r.Privileges = parsePrivileges(privs)
		r.GroupMappings = []GroupMapping{}
		for i := range min(len(providers), len(groups)) {
			r.GroupMappings = append(r.GroupMappings, GroupMapping{Provider: providers[i], Group: groups[i]})
		}
		return r, nil
	})
	return out, dbErr(err, "list roles")
}

// CreateRole creates a custom role (system:users:write).
func (s *Service) CreateRole(ctx context.Context, in RoleInput) (*Role, error) {
	if err := s.require(ctx, AuditRoleCreate, core.ActionWrite, core.SystemResource(core.SystemUsers)); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	var out *Role
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO roles (name, description) VALUES ($1, $2) RETURNING id::text`,
			in.Name, in.Description).Scan(&id); err != nil {
			return dbErr(err, fmt.Sprintf("create role %q", in.Name))
		}
		if err := setRoleContent(ctx, tx, id, in); err != nil {
			return err
		}
		var err error
		out, err = s.role(ctx, tx, in.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.rolesChanged(ctx)
	s.record(ctx, core.AuditEvent{
		Action: AuditRoleCreate, Target: "role:" + in.Name,
		Details: map[string]any{"privileges": privilegeStrings(in.Privileges), "group_mappings": nonNil(in.GroupMappings)},
	})
	return out, nil
}

// UpdateRole replaces a custom role's name, description, privileges and
// group mappings (system:users:write). Built-in roles return ErrBuiltinRole.
func (s *Service) UpdateRole(ctx context.Context, name string, in RoleInput) (*Role, error) {
	if err := s.require(ctx, AuditRoleUpdate, core.ActionWrite, core.SystemResource(core.SystemUsers)); err != nil {
		return nil, err
	}
	if err := in.validate(); err != nil {
		return nil, err
	}
	var out *Role
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var id string
		var builtin bool
		if err := tx.QueryRow(ctx, `SELECT id::text, builtin FROM roles WHERE name = $1 FOR UPDATE`, name).Scan(&id, &builtin); err != nil {
			return dbErr(err, fmt.Sprintf("role %q", name))
		}
		if builtin {
			return ErrBuiltinRole
		}
		if _, err := tx.Exec(ctx, `UPDATE roles SET name = $2, description = $3, updated_at = now() WHERE id = $1`,
			id, in.Name, in.Description); err != nil {
			return dbErr(err, fmt.Sprintf("update role %q", name))
		}
		if err := setRoleContent(ctx, tx, id, in); err != nil {
			return err
		}
		var err error
		out, err = s.role(ctx, tx, in.Name)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.rolesChanged(ctx)
	s.record(ctx, core.AuditEvent{
		Action: AuditRoleUpdate, Target: "role:" + in.Name,
		Details: map[string]any{"previous_name": name, "privileges": privilegeStrings(in.Privileges), "group_mappings": nonNil(in.GroupMappings)},
	})
	return out, nil
}

// DeleteRole deletes a custom role and its bindings (system:users:delete).
func (s *Service) DeleteRole(ctx context.Context, name string) error {
	if err := s.require(ctx, AuditRoleDelete, core.ActionDelete, core.SystemResource(core.SystemUsers)); err != nil {
		return err
	}
	var builtin bool
	err := s.pool.QueryRow(ctx, `SELECT builtin FROM roles WHERE name = $1`, name).Scan(&builtin)
	if err != nil {
		return dbErr(err, fmt.Sprintf("role %q", name))
	}
	if builtin {
		return ErrBuiltinRole
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM roles WHERE name = $1 AND NOT builtin`, name)
	if err != nil {
		return dbErr(err, "delete role")
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("role %q: %w", name, core.ErrNotFound)
	}
	s.rolesChanged(ctx)
	s.record(ctx, core.AuditEvent{Action: AuditRoleDelete, Target: "role:" + name})
	return nil
}

func setRoleContent(ctx context.Context, tx pgx.Tx, roleID string, in RoleInput) error {
	if _, err := tx.Exec(ctx, `DELETE FROM privileges WHERE role_id = $1`, roleID); err != nil {
		return dbErr(err, "replace privileges")
	}
	privs := slices.Compact(slices.Sorted(slices.Values(privilegeStrings(in.Privileges))))
	if _, err := tx.Exec(ctx, `INSERT INTO privileges (role_id, privilege) SELECT $1, unnest($2::text[])`, roleID, privs); err != nil {
		return dbErr(err, "replace privileges")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM role_bindings WHERE role_id = $1 AND subject_kind = 'group'`, roleID); err != nil {
		return dbErr(err, "replace group mappings")
	}
	for _, g := range in.GroupMappings {
		var providerID string
		err := tx.QueryRow(ctx, `SELECT id::text FROM identity_providers WHERE name = $1`, g.Provider).Scan(&providerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("identity provider %q does not exist: %w", g.Provider, core.ErrInvalid)
		}
		if err != nil {
			return dbErr(err, "load identity provider")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO role_bindings (role_id, subject_kind, group_name, provider_id) VALUES ($1, 'group', $2, $3)
			ON CONFLICT DO NOTHING`, roleID, g.Group, providerID); err != nil {
			return dbErr(err, "replace group mappings")
		}
	}
	return nil
}

// RolesForGroups returns the roles bound to any of groups of the named
// identity provider. External authenticators use it to fill Principal.Roles.
func (s *Service) RolesForGroups(ctx context.Context, provider string, groups []string) ([]string, error) {
	if len(groups) == 0 {
		return []string{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT r.name FROM role_bindings b
		JOIN roles r ON r.id = b.role_id
		JOIN identity_providers ip ON ip.id = b.provider_id
		WHERE b.subject_kind = 'group' AND ip.name = $1 AND b.group_name = ANY($2)
		ORDER BY 1`, provider, groups)
	if err != nil {
		return nil, dbErr(err, "roles for groups")
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return names, dbErr(err, "roles for groups")
}
