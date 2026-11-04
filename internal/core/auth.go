package core

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// PrincipalKind tells who is acting.
type PrincipalKind string

const (
	PrincipalAnonymous      PrincipalKind = "anonymous"
	PrincipalUser           PrincipalKind = "user"
	PrincipalServiceAccount PrincipalKind = "service_account"
)

// AnonymousName is the Principal.Name of unauthenticated requests.
const AnonymousName = "anonymous"

// Principal is an authenticated (or anonymous) actor.
type Principal struct {
	ID   string        `json:"id,omitempty"`
	Name string        `json:"name"` // username or service account name; "anonymous"
	Kind PrincipalKind `json:"kind"`
	// Email and DisplayName are informational.
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	// Source is where the identity came from: "local", "oidc", "ldap", "saml", "token".
	Source string `json:"source,omitempty"`
	// Roles are role names bound to the principal (directly or via groups).
	Roles []string `json:"roles,omitempty"`
	// Scopes, when non-nil, restrict a token-authenticated request to these
	// privileges in addition to the principal's roles (intersection).
	Scopes []Privilege `json:"scopes,omitempty"`
	// TokenID is set when the request authenticated with a token.
	TokenID string `json:"token_id,omitempty"`
	// Claims are the TokenRequest.Extra values of the session token used, if any.
	Claims map[string]any `json:"claims,omitempty"`
	// PasswordChangeRequired marks a local user who signed in with a
	// password that must be replaced first. Such a principal is authorized
	// for nothing; only the password change itself is open to it.
	PasswordChangeRequired bool `json:"-"`
}

// Anonymous returns the principal for requests without credentials.
func Anonymous() *Principal {
	return &Principal{Name: AnonymousName, Kind: PrincipalAnonymous}
}

// IsAnonymous reports whether p is nil or the anonymous principal.
func (p *Principal) IsAnonymous() bool { return p == nil || p.Kind == PrincipalAnonymous }

// Action is what a principal wants to do with a resource.
type Action string

const (
	ActionRead   Action = "read"
	ActionWrite  Action = "write"
	ActionDelete Action = "delete"
	// ActionAdmin implies every other action on the same resource.
	ActionAdmin Action = "admin"
	// ActionAny matches every action in a Privilege pattern ("system:*").
	ActionAny Action = "*"
)

// Domain is the top-level resource class of a privilege.
type Domain string

const (
	// DomainRepo: a repository, by name.
	DomainRepo Domain = "repo"
	// DomainSystem: an administrative area, see the System* constants.
	DomainSystem Domain = "system"
)

// System areas used with DomainSystem.
const (
	SystemUsers      = "users"      // users, roles, role bindings, identity providers
	SystemTokens     = "tokens"     // other principals' tokens (own tokens need no privilege)
	SystemRepos      = "repos"      // create/delete repositories
	SystemBlobStores = "blobstores" // blob store config, moves
	SystemTasks      = "tasks"      // scheduled tasks, run/stop
	SystemCleanup    = "cleanup"    // cleanup policies
	SystemSecurity   = "security"   // findings, waivers, scan policy
	SystemAudit      = "audit"      // read audit log
	SystemCluster    = "cluster"    // nodes, replication
	SystemSettings   = "settings"   // global settings
)

// Resource is the target of an authorization check.
type Resource struct {
	Domain Domain `json:"domain"`
	Name   string `json:"name"` // repository name or system area
}

// RepoResource returns the resource for repository name.
func RepoResource(name string) Resource { return Resource{Domain: DomainRepo, Name: name} }

// SystemResource returns the resource for a system area (see System* constants).
func SystemResource(area string) Resource { return Resource{Domain: DomainSystem, Name: area} }

// String renders "repo:npm-hosted".
func (r Resource) String() string { return string(r.Domain) + ":" + r.Name }

// Privilege grants Action on resources whose name matches Pattern:
// "repo:<pattern>:<action>" or "system:<area>:<action>". Pattern is a path.Match
// glob ("*", "npm-*"). The short form "system:*" means every system area and action.
// Examples: "repo:*:read", "repo:npm-*:write", "repo:docker-hosted:admin", "system:users:admin".
type Privilege struct {
	Domain  Domain `json:"domain"`
	Pattern string `json:"pattern"`
	Action  Action `json:"action"`
}

// RepoPrivilege is shorthand for the privilege "repo:<repo>:<action>".
func RepoPrivilege(repo string, a Action) Privilege {
	return Privilege{Domain: DomainRepo, Pattern: repo, Action: a}
}

// ParsePrivilege parses the string form described on Privilege.
func ParsePrivilege(s string) (Privilege, error) {
	parts := strings.Split(s, ":")
	if len(parts) == 2 && parts[0] == string(DomainSystem) && parts[1] == "*" {
		return Privilege{Domain: DomainSystem, Pattern: "*", Action: ActionAny}, nil
	}
	if len(parts) != 3 {
		return Privilege{}, fmt.Errorf("privilege %q: want <domain>:<pattern>:<action>: %w", s, ErrInvalid)
	}
	p := Privilege{Domain: Domain(parts[0]), Pattern: parts[1], Action: Action(parts[2])}
	if p.Domain != DomainRepo && p.Domain != DomainSystem {
		return Privilege{}, fmt.Errorf("privilege %q: unknown domain %q: %w", s, parts[0], ErrInvalid)
	}
	if p.Pattern == "" {
		return Privilege{}, fmt.Errorf("privilege %q: empty pattern: %w", s, ErrInvalid)
	}
	if _, err := path.Match(p.Pattern, ""); err != nil {
		return Privilege{}, fmt.Errorf("privilege %q: bad pattern: %w", s, ErrInvalid)
	}
	switch p.Action {
	case ActionRead, ActionWrite, ActionDelete, ActionAdmin, ActionAny:
	default:
		return Privilege{}, fmt.Errorf("privilege %q: unknown action %q: %w", s, parts[2], ErrInvalid)
	}
	return p, nil
}

// MustParsePrivilege is ParsePrivilege for constants; it panics on error.
func MustParsePrivilege(s string) Privilege {
	p, err := ParsePrivilege(s)
	if err != nil {
		panic(err)
	}
	return p
}

// String renders the canonical form.
func (p Privilege) String() string {
	return string(p.Domain) + ":" + p.Pattern + ":" + string(p.Action)
}

// MarshalText implements encoding.TextMarshaler (privileges serialise as strings).
func (p Privilege) MarshalText() ([]byte, error) { return []byte(p.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (p *Privilege) UnmarshalText(b []byte) error {
	v, err := ParsePrivilege(string(b))
	if err != nil {
		return err
	}
	*p = v
	return nil
}

// Allows reports whether the privilege grants action on res.
// admin and * imply read, write and delete.
func (p Privilege) Allows(action Action, res Resource) bool {
	if p.Domain != res.Domain {
		return false
	}
	if ok, _ := path.Match(p.Pattern, res.Name); !ok {
		return false
	}
	return p.Action == action || p.Action == ActionAdmin || p.Action == ActionAny
}

// Token is a personal access token or service-account token. The secret is
// shown once at creation; only its SHA-256 hash is stored. Secrets start with
// TokenPrefix so secret scanners can find leaked ones.
type Token struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Owner      string      `json:"owner"` // principal name
	Prefix     string      `json:"prefix"`
	Scopes     []Privilege `json:"scopes"`
	ExpiresAt  time.Time   `json:"expires_at"`
	CreatedAt  time.Time   `json:"created_at"`
	LastUsedAt *time.Time  `json:"last_used_at,omitempty"`
}

// TokenPrefix starts every secret Hangar issues.
const TokenPrefix = "hgr_"
