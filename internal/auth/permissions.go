package auth

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// maxPrivileges bounds the privileges of a role and the scopes of a token.
const maxPrivileges = 100

// SystemAreas are the administrative areas a system privilege can name.
var SystemAreas = []string{
	core.SystemUsers, core.SystemTokens, core.SystemRepos, core.SystemBlobStores, core.SystemTasks,
	core.SystemCleanup, core.SystemSecurity, core.SystemAudit, core.SystemCluster, core.SystemSettings,
}

// Actions in the order they are checked and shown.
var Actions = []core.Action{core.ActionRead, core.ActionWrite, core.ActionDelete, core.ActionAdmin}

// repoPatternRE allows repository names and glob characters.
var repoPatternRE = regexp.MustCompile(`^[a-z0-9*?\[\]^!-]{1,64}$`)

// ValidatePrivileges rejects privileges that can never match anything: a
// system area that does not exist, or a repository pattern with characters
// repository names cannot contain.
func ValidatePrivileges(ps []core.Privilege) error {
	for _, p := range ps {
		switch p.Domain {
		case core.DomainSystem:
			if p.Pattern != "*" && !slices.Contains(SystemAreas, p.Pattern) {
				return fmt.Errorf("privilege %s: unknown system area %q (want * or one of %v): %w", p, p.Pattern, SystemAreas, core.ErrInvalid)
			}
		case core.DomainRepo:
			if p.Action == core.ActionAny {
				return fmt.Errorf("privilege %s: use admin for every action on repositories: %w", p, core.ErrInvalid)
			}
			if !repoPatternRE.MatchString(p.Pattern) {
				return fmt.Errorf("privilege %s: repository pattern must be a repository name or glob (a-z, 0-9, -, *, ?): %w", p, core.ErrInvalid)
			}
		default:
			return fmt.Errorf("privilege %s: unknown domain: %w", p, core.ErrInvalid)
		}
	}
	return nil
}

// compactPrivileges sorts and de-duplicates privileges by their string form.
func compactPrivileges(ps []core.Privilege) []core.Privilege {
	out := slices.Clone(ps)
	slices.SortFunc(out, func(a, b core.Privilege) int {
		switch {
		case a.String() < b.String():
			return -1
		case a.String() > b.String():
			return 1
		}
		return 0
	})
	return slices.CompactFunc(out, func(a, b core.Privilege) bool { return a == b })
}

// Grant explains why an action is allowed: a privilege of one of the user's
// roles, or the repository's anonymous read setting.
type Grant struct {
	Action core.Action `json:"action"`
	// Source is "role" or "anonymous".
	Source    string          `json:"source"`
	Role      string          `json:"role,omitempty"`
	Privilege *core.Privilege `json:"privilege,omitempty"`
}

// Grant sources.
const (
	GrantRole      = "role"
	GrantAnonymous = "anonymous"
)

// ResourcePermissions are the actions a user may take on one resource and
// the grants behind them. Resources the user cannot touch at all are left out.
type ResourcePermissions struct {
	// Resource is "repo:<name>" or "system:<area>".
	Resource string `json:"resource"`
	Name     string `json:"name"`
	// Format and Type are set for repositories.
	Format string `json:"format,omitempty"`
	Type   string `json:"type,omitempty"`
	// AnonymousRead is set for repositories that everyone may read.
	AnonymousRead bool          `json:"anonymous_read,omitempty"`
	Actions       []core.Action `json:"actions"`
	Grants        []Grant       `json:"grants"`
}

// RoleGrant is one of the user's roles with its privileges.
type RoleGrant struct {
	Name       string           `json:"name"`
	Builtin    bool             `json:"builtin"`
	Privileges []core.Privilege `json:"privileges"`
}

// EffectivePermissions answers "what may this user do, and why" for audits.
type EffectivePermissions struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Kind     string `json:"kind"`
	// Disabled and Locked accounts can do nothing at the moment, whatever
	// their roles say; the lists still show what applies once re-enabled.
	Disabled     bool                  `json:"disabled"`
	Locked       bool                  `json:"locked"`
	Roles        []RoleGrant           `json:"roles"`
	Repositories []ResourcePermissions `json:"repositories"`
	System       []ResourcePermissions `json:"system"`
	// Notes explain what the lists cannot show.
	Notes []string `json:"notes"`
}

// maxPermissionRepos bounds the repositories evaluated for one report.
const maxPermissionRepos = 10_000

// EffectivePermissions reports, for every repository and system area, what
// the user may do through their directly bound roles and anonymous access
// (system:users:read, or the user themself).
func (s *Service) EffectivePermissions(ctx context.Context, userID string) (*EffectivePermissions, error) {
	if userID != core.PrincipalFrom(ctx).ID {
		if err := s.require(ctx, "user.permissions", core.ActionRead, core.SystemResource(core.SystemUsers)); err != nil {
			return nil, err
		}
	}
	u, err := s.userByID(ctx, s.pool, userID)
	if err != nil {
		return nil, err
	}
	roles, err := s.roleGrants(ctx, u.Roles)
	if err != nil {
		return nil, err
	}
	out := &EffectivePermissions{
		UserID: u.ID, Username: u.Username, Kind: u.Kind, Disabled: u.Disabled, Locked: u.lockedAt(s.now()),
		Roles: roles, Repositories: []ResourcePermissions{}, System: []ResourcePermissions{}, Notes: []string{},
	}
	if u.Source != SourceLocal {
		out.Notes = append(out.Notes, fmt.Sprintf("Roles granted through %s group mappings are applied at sign-in and are not included.", u.Source))
	}
	out.Notes = append(out.Notes, "Personal access tokens are further limited to their scopes.")

	repos, truncated, err := s.allRepositories(ctx)
	if err != nil {
		return nil, err
	}
	if truncated {
		out.Notes = append(out.Notes, fmt.Sprintf("Only the first %d repositories were evaluated.", maxPermissionRepos))
	}
	for _, r := range repos {
		rp := evaluate(roles, core.RepoResource(r.Name), r.Config.AnonymousRead)
		if len(rp.Actions) == 0 {
			continue
		}
		rp.Format, rp.Type, rp.AnonymousRead = r.Format, string(r.Type), r.Config.AnonymousRead
		out.Repositories = append(out.Repositories, rp)
	}
	for _, area := range SystemAreas {
		if rp := evaluate(roles, core.SystemResource(area), false); len(rp.Actions) > 0 {
			out.System = append(out.System, rp)
		}
	}
	return out, nil
}

// evaluate lists the allowed actions on res and every grant behind them.
func evaluate(roles []RoleGrant, res core.Resource, anonymousRead bool) ResourcePermissions {
	rp := ResourcePermissions{Resource: res.String(), Name: res.Name, Actions: []core.Action{}, Grants: []Grant{}}
	for _, a := range Actions {
		allowed := false
		for _, r := range roles {
			for _, p := range r.Privileges {
				if p.Allows(a, res) {
					allowed = true
					rp.Grants = append(rp.Grants, Grant{Action: a, Source: GrantRole, Role: r.Name, Privilege: &p})
				}
			}
		}
		if a == core.ActionRead && res.Domain == core.DomainRepo && anonymousRead {
			allowed = true
			rp.Grants = append(rp.Grants, Grant{Action: a, Source: GrantAnonymous})
		}
		if allowed {
			rp.Actions = append(rp.Actions, a)
		}
	}
	return rp
}

// roleGrants loads the named roles with their privileges, by name.
func (s *Service) roleGrants(ctx context.Context, names []string) ([]RoleGrant, error) {
	out := []RoleGrant{}
	if len(names) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.name, r.builtin,
			coalesce((SELECT array_agg(p.privilege ORDER BY p.privilege) FROM privileges p WHERE p.role_id = r.id), '{}')
		FROM roles r WHERE r.name = ANY($1) ORDER BY r.name`, names)
	if err != nil {
		return nil, dbErr(err, "load roles")
	}
	out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RoleGrant, error) {
		var g RoleGrant
		var privs []string
		err := row.Scan(&g.Name, &g.Builtin, &privs)
		g.Privileges = parsePrivileges(privs)
		return g, err
	})
	return nonNil(out), dbErr(err, "load roles")
}

// allRepositories pages through the repositories, by name, up to
// maxPermissionRepos.
func (s *Service) allRepositories(ctx context.Context) ([]*core.Repository, bool, error) {
	g := s.authz.repos.Load()
	if g == nil {
		return nil, false, nil
	}
	var out []*core.Repository
	q := core.RepositoryQuery{Limit: core.MaxPageSize}
	for {
		p, err := g.List(ctx, q)
		if err != nil {
			return nil, false, fmt.Errorf("list repositories: %w", err)
		}
		out = append(out, p.Items...)
		if len(out) >= maxPermissionRepos {
			return out[:maxPermissionRepos], true, nil
		}
		if p.NextCursor == "" {
			return out, false, nil
		}
		q.Cursor = p.NextCursor
	}
}
