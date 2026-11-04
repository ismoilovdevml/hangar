package auth

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// OIDCConfig configures sign-in with an OpenID Connect provider
// (authorization code flow with PKCE). The redirect URI registered at the
// provider is server.base_url + /api/v1/auth/oidc/callback.
type OIDCConfig struct {
	Enabled bool `yaml:"enabled"`
	// IssuerURL is the issuer identifier; discovery reads
	// IssuerURL + /.well-known/openid-configuration.
	IssuerURL string `yaml:"issuer_url"`
	ClientID  string `yaml:"client_id"`
	// ClientSecret of a confidential client (HANGAR_AUTH_OIDC_CLIENT_SECRET).
	// Empty for a public client, which then relies on PKCE alone.
	ClientSecret string `yaml:"client_secret"` // from env; masked by Redacted
	// ClientAuth is how the client secret reaches the token endpoint:
	// "basic" (client_secret_basic, default) or "post" (client_secret_post).
	ClientAuth string `yaml:"client_auth"`
	// DisplayName labels the "Continue with ..." button.
	DisplayName string   `yaml:"display_name"`
	Scopes      []string `yaml:"scopes"`
	// Claims read from the ID token (and from the userinfo endpoint when the
	// ID token lacks them). A dotted path reaches into nested objects
	// ("realm_access.roles").
	UsernameClaim string `yaml:"username_claim"`
	EmailClaim    string `yaml:"email_claim"`
	NameClaim     string `yaml:"name_claim"`
	GroupsClaim   string `yaml:"groups_claim"`

	// GroupRoles, DefaultRoles, AllowedGroups and AutoCreate: see RoleMapping.
	GroupRoles    map[string][]string `yaml:"group_roles"`
	DefaultRoles  []string            `yaml:"default_roles"`
	AllowedGroups []string            `yaml:"allowed_groups"`
	AutoCreate    bool                `yaml:"auto_create"`

	// InsecureSkipVerify disables TLS verification of the provider. Never in
	// production; use CAFile for a private CA instead.
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	CAFile             string `yaml:"ca_file"`
	// Timeout bounds each request to the provider.
	Timeout time.Duration `yaml:"timeout"`
}

// LDAPConfig configures username/password sign-in against an LDAP directory
// or Active Directory, on the login form (realm "ldap") and with HTTP Basic.
type LDAPConfig struct {
	Enabled bool `yaml:"enabled"`
	// URL is ldap://host:389 or ldaps://host:636.
	URL string `yaml:"url"`
	// StartTLS upgrades an ldap:// connection before any bind.
	StartTLS           bool   `yaml:"start_tls"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	CAFile             string `yaml:"ca_file"`
	// BindDN and BindPassword (HANGAR_AUTH_LDAP_BIND_PASSWORD) are the service
	// account that searches for users and groups. Both empty: anonymous search.
	BindDN       string `yaml:"bind_dn"`
	BindPassword string `yaml:"bind_password"` // from env; masked by Redacted

	UserBaseDN string `yaml:"user_base_dn"`
	// UserFilter finds the user; {username} is replaced by the escaped login name.
	UserFilter        string `yaml:"user_filter"`
	UsernameAttribute string `yaml:"username_attribute"`
	EmailAttribute    string `yaml:"email_attribute"`
	NameAttribute     string `yaml:"name_attribute"`

	// Groups come from GroupAttribute on the user entry (memberOf) unless
	// GroupBaseDN is set; then GroupFilter is searched under it ({dn} is the
	// user's escaped DN, {username} the login name) and GroupNameAttribute
	// names each group found.
	GroupAttribute     string `yaml:"group_attribute"`
	GroupBaseDN        string `yaml:"group_base_dn"`
	GroupFilter        string `yaml:"group_filter"`
	GroupNameAttribute string `yaml:"group_name_attribute"`
	// NestedGroups resolves Active Directory nested groups with
	// LDAP_MATCHING_RULE_IN_CHAIN. It needs GroupBaseDN; the default group
	// filter becomes (member:1.2.840.113556.1.4.1941:={dn}).
	NestedGroups bool `yaml:"nested_groups"`

	// GroupRoles, DefaultRoles, AllowedGroups and AutoCreate: see RoleMapping.
	GroupRoles    map[string][]string `yaml:"group_roles"`
	DefaultRoles  []string            `yaml:"default_roles"`
	AllowedGroups []string            `yaml:"allowed_groups"`
	AutoCreate    bool                `yaml:"auto_create"`

	// Timeout bounds connecting and each LDAP operation.
	Timeout time.Duration `yaml:"timeout"`
}

// Default LDAP settings (Active Directory).
const (
	DefaultLDAPUserFilter        = "(&(objectClass=user)(sAMAccountName={username}))"
	DefaultLDAPGroupFilter       = "(member={dn})"
	DefaultLDAPNestedGroupFilter = "(member:1.2.840.113556.1.4.1941:={dn})"
	defaultExternalTimeout       = 10 * time.Second
	maxGroupRoleEntries          = 1000
)

// RoleMapping turns the groups of an external identity into roles.
type RoleMapping struct {
	// GroupRoles maps a group name to the roles its members get. Names match
	// case-insensitively and with or without a leading "/" (Keycloak group
	// paths); for LDAP both the group's DN and its name match.
	GroupRoles map[string][]string
	// DefaultRoles are granted to every user of the provider.
	DefaultRoles []string
	// AllowedGroups, when not empty, admits only members of these groups.
	AllowedGroups []string
	// AutoCreate creates users on their first sign-in. Without it only users
	// who signed in before are admitted.
	AutoCreate bool
}

// Mapping returns the role mapping of the OIDC provider.
func (c OIDCConfig) Mapping() RoleMapping {
	return RoleMapping{GroupRoles: c.GroupRoles, DefaultRoles: c.DefaultRoles, AllowedGroups: c.AllowedGroups, AutoCreate: c.AutoCreate}
}

// Mapping returns the role mapping of the LDAP directory.
func (c LDAPConfig) Mapping() RoleMapping {
	return RoleMapping{GroupRoles: c.GroupRoles, DefaultRoles: c.DefaultRoles, AllowedGroups: c.AllowedGroups, AutoCreate: c.AutoCreate}
}

// sameGroup compares group names case-insensitively, ignoring a leading
// "/": Keycloak sends full group paths ("/devops", "/it/devops").
func sameGroup(a, b string) bool {
	return strings.EqualFold(a, b) || strings.EqualFold(strings.TrimPrefix(a, "/"), strings.TrimPrefix(b, "/"))
}

// GroupVariants returns groups plus each name without its leading "/", for
// exact lookups such as RoleInput.GroupMappings.
func GroupVariants(groups []string) []string {
	out := append([]string{}, groups...)
	for _, g := range groups {
		if t := strings.TrimPrefix(g, "/"); t != g && t != "" {
			out = append(out, t)
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(out)))
}

// Allows reports whether a member of groups may sign in.
func (m RoleMapping) Allows(groups []string) bool {
	if len(m.AllowedGroups) == 0 {
		return true
	}
	for _, a := range m.AllowedGroups {
		for _, g := range groups {
			if sameGroup(a, g) {
				return true
			}
		}
	}
	return false
}

// Roles returns the sorted roles granted to a member of groups.
func (m RoleMapping) Roles(groups []string) []string {
	out := append([]string{}, m.DefaultRoles...)
	for name, roles := range m.GroupRoles {
		for _, g := range groups {
			if sameGroup(name, g) {
				out = append(out, roles...)
				break
			}
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(out)))
}

// RoleNames lists every role the mapping refers to.
func (m RoleMapping) RoleNames() []string {
	out := append([]string{}, m.DefaultRoles...)
	for _, roles := range m.GroupRoles {
		out = append(out, roles...)
	}
	return slices.Compact(slices.Sorted(slices.Values(out)))
}

func (m RoleMapping) validate(prefix string) []error {
	var errs []error
	if len(m.GroupRoles) > maxGroupRoleEntries {
		errs = append(errs, fmt.Errorf("%s.group_roles has more than %d entries", prefix, maxGroupRoleEntries))
	}
	for group, roles := range m.GroupRoles {
		if strings.TrimSpace(group) == "" {
			errs = append(errs, fmt.Errorf("%s.group_roles: empty group name", prefix))
		}
		if len(roles) == 0 {
			errs = append(errs, fmt.Errorf("%s.group_roles[%q]: no roles listed", prefix, group))
		}
	}
	for _, r := range m.RoleNames() {
		if !roleNameRE.MatchString(r) {
			errs = append(errs, fmt.Errorf("%s: role name %q is not valid", prefix, r))
		}
	}
	for _, g := range m.AllowedGroups {
		if strings.TrimSpace(g) == "" {
			errs = append(errs, fmt.Errorf("%s.allowed_groups: empty group name", prefix))
		}
	}
	return errs
}

func (c OIDCConfig) withDefaults() OIDCConfig {
	if c.DisplayName == "" {
		c.DisplayName = "SSO"
	}
	if c.ClientAuth == "" {
		c.ClientAuth = "basic"
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{"openid", "profile", "email"}
	}
	if c.UsernameClaim == "" {
		c.UsernameClaim = "preferred_username"
	}
	if c.EmailClaim == "" {
		c.EmailClaim = "email"
	}
	if c.NameClaim == "" {
		c.NameClaim = "name"
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultExternalTimeout
	}
	return c
}

func (c LDAPConfig) withDefaults() LDAPConfig {
	if c.UserFilter == "" {
		c.UserFilter = DefaultLDAPUserFilter
	}
	if c.UsernameAttribute == "" {
		c.UsernameAttribute = "sAMAccountName"
	}
	if c.EmailAttribute == "" {
		c.EmailAttribute = "mail"
	}
	if c.NameAttribute == "" {
		c.NameAttribute = "displayName"
	}
	if c.GroupAttribute == "" {
		c.GroupAttribute = "memberOf"
	}
	if c.GroupFilter == "" {
		c.GroupFilter = DefaultLDAPGroupFilter
		if c.NestedGroups {
			c.GroupFilter = DefaultLDAPNestedGroupFilter
		}
	}
	if c.GroupNameAttribute == "" {
		c.GroupNameAttribute = "cn"
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultExternalTimeout
	}
	return c
}

func (c OIDCConfig) validate() []error {
	if !c.Enabled {
		return nil
	}
	var errs []error
	u, err := url.Parse(c.IssuerURL)
	switch {
	case c.IssuerURL == "":
		errs = append(errs, errors.New("auth.oidc.issuer_url is required when auth.oidc.enabled is true"))
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		errs = append(errs, fmt.Errorf("auth.oidc.issuer_url %q must be an absolute https:// URL", c.IssuerURL))
	case u.Scheme == "http" && !isLoopbackHost(u.Hostname()):
		errs = append(errs, fmt.Errorf("auth.oidc.issuer_url %q must use https (http is accepted for localhost only)", c.IssuerURL))
	}
	if c.ClientID == "" {
		errs = append(errs, errors.New("auth.oidc.client_id is required when auth.oidc.enabled is true"))
	}
	if c.ClientAuth != "basic" && c.ClientAuth != "post" {
		errs = append(errs, fmt.Errorf("auth.oidc.client_auth %q: want basic or post", c.ClientAuth))
	}
	if !slices.Contains(c.Scopes, "openid") {
		errs = append(errs, errors.New("auth.oidc.scopes must include openid"))
	}
	if len(c.DisplayName) > 64 {
		errs = append(errs, errors.New("auth.oidc.display_name is longer than 64 characters"))
	}
	return append(errs, c.Mapping().validate("auth.oidc")...)
}

func (c LDAPConfig) validate() []error {
	if !c.Enabled {
		return nil
	}
	var errs []error
	u, err := url.Parse(c.URL)
	switch {
	case c.URL == "":
		errs = append(errs, errors.New("auth.ldap.url is required when auth.ldap.enabled is true"))
	case err != nil || u.Host == "" || (u.Scheme != "ldap" && u.Scheme != "ldaps"):
		errs = append(errs, fmt.Errorf("auth.ldap.url %q must be ldap://host[:port] or ldaps://host[:port]", c.URL))
	case u.Scheme == "ldaps" && c.StartTLS:
		errs = append(errs, errors.New("auth.ldap.start_tls cannot be combined with an ldaps:// url"))
	}
	if (c.BindDN == "") != (c.BindPassword == "") {
		errs = append(errs, errors.New("auth.ldap.bind_dn and auth.ldap.bind_password (HANGAR_AUTH_LDAP_BIND_PASSWORD) must be set together"))
	}
	if c.UserBaseDN == "" {
		errs = append(errs, errors.New("auth.ldap.user_base_dn is required when auth.ldap.enabled is true"))
	}
	if !strings.Contains(c.UserFilter, "{username}") {
		errs = append(errs, fmt.Errorf("auth.ldap.user_filter %q must contain {username}", c.UserFilter))
	}
	if !strings.HasPrefix(c.UserFilter, "(") || !strings.HasPrefix(c.GroupFilter, "(") {
		errs = append(errs, errors.New("auth.ldap.user_filter and group_filter must be LDAP filters in parentheses"))
	}
	if c.NestedGroups && c.GroupBaseDN == "" {
		errs = append(errs, errors.New("auth.ldap.nested_groups needs auth.ldap.group_base_dn"))
	}
	return append(errs, c.Mapping().validate("auth.ldap")...)
}

// ExternalEnabled reports whether any external sign-in method is on.
func (c Config) ExternalEnabled() bool { return c.OIDC.Enabled || c.LDAP.Enabled }

// validateExternal joins the OIDC and LDAP configuration errors.
func (c Config) validateExternal() error {
	errs := append(c.OIDC.validate(), c.LDAP.validate()...)
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", errors.Join(errs...), core.ErrInvalid)
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
