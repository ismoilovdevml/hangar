package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// Keys of the settings table.
const (
	keyOIDC = "auth.oidc"
	keyLDAP = "auth.ldap"
)

// GroupRole grants Roles to members of Group (one row of the mapping table).
type GroupRole struct {
	Group string   `json:"group"`
	Roles []string `json:"roles"`
}

// Mapping is the group-to-role mapping of a provider (auth.RoleMapping).
type Mapping struct {
	GroupRoles    []GroupRole `json:"group_roles"`
	DefaultRoles  []string    `json:"default_roles"`
	AllowedGroups []string    `json:"allowed_groups"`
	AutoCreate    bool        `json:"auto_create"`
}

// OIDCSettings are the editable OIDC settings (schema OIDCSettings).
type OIDCSettings struct {
	Enabled       bool     `json:"enabled"`
	IssuerURL     string   `json:"issuer_url"`
	ClientID      string   `json:"client_id"`
	ClientAuth    string   `json:"client_auth"`
	DisplayName   string   `json:"display_name"`
	Scopes        []string `json:"scopes"`
	UsernameClaim string   `json:"username_claim"`
	EmailClaim    string   `json:"email_claim"`
	NameClaim     string   `json:"name_claim"`
	GroupsClaim   string   `json:"groups_claim"`
	CAPEM         string   `json:"ca_pem"`
	// Preset names the provider template the settings page filled the
	// form from ("keycloak", "entra-id", ...). Informational only: it
	// lets the page reopen on the same template.
	Preset string `json:"preset"`
	Mapping
}

// LDAPSettings are the editable LDAP settings (schema LDAPSettings).
type LDAPSettings struct {
	Enabled            bool   `json:"enabled"`
	URL                string `json:"url"`
	StartTLS           bool   `json:"start_tls"`
	CAPEM              string `json:"ca_pem"`
	BindDN             string `json:"bind_dn"`
	UserBaseDN         string `json:"user_base_dn"`
	UserFilter         string `json:"user_filter"`
	UsernameAttribute  string `json:"username_attribute"`
	EmailAttribute     string `json:"email_attribute"`
	NameAttribute      string `json:"name_attribute"`
	GroupAttribute     string `json:"group_attribute"`
	GroupBaseDN        string `json:"group_base_dn"`
	GroupFilter        string `json:"group_filter"`
	GroupNameAttribute string `json:"group_name_attribute"`
	NestedGroups       bool   `json:"nested_groups"`
	// Preset is like OIDCSettings.Preset ("active-directory", ...).
	Preset string `json:"preset"`
	Mapping
}

// Meta says where a setting comes from.
type Meta struct {
	// Source is SourceFile, SourceDatabase or SourceDefault.
	Source string `json:"source"`
	// FileFields are the fields the configuration file sets (read-only).
	FileFields []string   `json:"file_fields"`
	UpdatedAt  *time.Time `json:"updated_at"`
	UpdatedBy  string     `json:"updated_by,omitempty"`
}

// OIDC is the OIDC provider as GET returns it (schema OIDCProviderSettings).
type OIDC struct {
	OIDCSettings
	ClientSecretConfigured bool `json:"client_secret_configured"`
	// CAFile is auth.oidc.ca_file of a provider managed by the file.
	CAFile string `json:"ca_file,omitempty"`
	// RedirectURI is the callback URL to register at the provider.
	RedirectURI string `json:"redirect_uri"`
	Meta
}

// OIDCInput is the body of PUT /identity-providers/oidc. An empty
// ClientSecret keeps the stored one; ClearClientSecret removes it.
type OIDCInput struct {
	OIDCSettings
	ClientSecret      string `json:"client_secret"`
	ClearClientSecret bool   `json:"clear_client_secret"`
}

// LDAP is the LDAP directory as GET returns it (schema LDAPProviderSettings).
type LDAP struct {
	LDAPSettings
	BindPasswordConfigured bool   `json:"bind_password_configured"`
	CAFile                 string `json:"ca_file,omitempty"`
	Meta
}

// LDAPInput is the body of PUT /identity-providers/ldap. An empty
// BindPassword keeps the stored one; ClearBindPassword removes it.
type LDAPInput struct {
	LDAPSettings
	BindPassword      string `json:"bind_password"`
	ClearBindPassword bool   `json:"clear_bind_password"`
}

// oidcDoc and ldapDoc are the stored documents; the secret is sealed.
type oidcDoc struct {
	OIDCSettings
	ClientSecret string `json:"client_secret,omitempty"`
}

type ldapDoc struct {
	LDAPSettings
	BindPassword string `json:"bind_password,omitempty"`
}

var (
	oidcFields = []string{"enabled", "issuer_url", "client_id", "client_secret", "client_auth", "display_name", "scopes",
		"username_claim", "email_claim", "name_claim", "groups_claim", "ca_pem", "group_roles", "default_roles", "allowed_groups", "auto_create"}
	ldapFields = []string{"enabled", "url", "start_tls", "ca_pem", "bind_dn", "bind_password", "user_base_dn", "user_filter",
		"username_attribute", "email_attribute", "name_attribute", "group_attribute", "group_base_dn", "group_filter",
		"group_name_attribute", "nested_groups", "group_roles", "default_roles", "allowed_groups", "auto_create"}
)

// ---------------------------------------------------------------- conversions

func (m Mapping) roleMapping() (map[string][]string, []string, []string, bool) {
	var gr map[string][]string
	if len(m.GroupRoles) > 0 {
		gr = make(map[string][]string, len(m.GroupRoles))
		for _, r := range m.GroupRoles {
			gr[r.Group] = append(gr[r.Group], r.Roles...)
		}
	}
	return gr, slices.Clone(m.DefaultRoles), slices.Clone(m.AllowedGroups), m.AutoCreate
}

func mappingFrom(m auth.RoleMapping) Mapping {
	out := Mapping{GroupRoles: []GroupRole{}, DefaultRoles: nonNil(m.DefaultRoles), AllowedGroups: nonNil(m.AllowedGroups), AutoCreate: m.AutoCreate}
	for _, g := range slices.Sorted(maps.Keys(m.GroupRoles)) {
		out.GroupRoles = append(out.GroupRoles, GroupRole{Group: g, Roles: nonNil(m.GroupRoles[g])})
	}
	return out
}

func (o OIDCSettings) config(secret string) auth.OIDCConfig {
	c := auth.OIDCConfig{
		Enabled: o.Enabled, IssuerURL: o.IssuerURL, ClientID: o.ClientID, ClientSecret: secret, ClientAuth: o.ClientAuth,
		DisplayName: o.DisplayName, Scopes: slices.Clone(o.Scopes), UsernameClaim: o.UsernameClaim, EmailClaim: o.EmailClaim,
		NameClaim: o.NameClaim, GroupsClaim: o.GroupsClaim, CAPEM: o.CAPEM,
	}
	c.GroupRoles, c.DefaultRoles, c.AllowedGroups, c.AutoCreate = o.roleMapping()
	return c.WithDefaults()
}

func oidcSettingsFrom(c auth.OIDCConfig) OIDCSettings {
	c = c.WithDefaults()
	return OIDCSettings{
		Enabled: c.Enabled, IssuerURL: c.IssuerURL, ClientID: c.ClientID, ClientAuth: c.ClientAuth, DisplayName: c.DisplayName,
		Scopes: nonNil(c.Scopes), UsernameClaim: c.UsernameClaim, EmailClaim: c.EmailClaim, NameClaim: c.NameClaim,
		GroupsClaim: c.GroupsClaim, CAPEM: c.CAPEM, Mapping: mappingFrom(c.Mapping()),
	}
}

func (l LDAPSettings) config(password string) auth.LDAPConfig {
	c := auth.LDAPConfig{
		Enabled: l.Enabled, URL: l.URL, StartTLS: l.StartTLS, CAPEM: l.CAPEM, BindDN: l.BindDN, BindPassword: password,
		UserBaseDN: l.UserBaseDN, UserFilter: l.UserFilter, UsernameAttribute: l.UsernameAttribute,
		EmailAttribute: l.EmailAttribute, NameAttribute: l.NameAttribute, GroupAttribute: l.GroupAttribute,
		GroupBaseDN: l.GroupBaseDN, GroupFilter: l.GroupFilter, GroupNameAttribute: l.GroupNameAttribute,
		NestedGroups: l.NestedGroups,
	}
	c.GroupRoles, c.DefaultRoles, c.AllowedGroups, c.AutoCreate = l.roleMapping()
	return c.WithDefaults()
}

func ldapSettingsFrom(c auth.LDAPConfig) LDAPSettings {
	c = c.WithDefaults()
	return LDAPSettings{
		Enabled: c.Enabled, URL: c.URL, StartTLS: c.StartTLS, CAPEM: c.CAPEM, BindDN: c.BindDN, UserBaseDN: c.UserBaseDN,
		UserFilter: c.UserFilter, UsernameAttribute: c.UsernameAttribute, EmailAttribute: c.EmailAttribute,
		NameAttribute: c.NameAttribute, GroupAttribute: c.GroupAttribute, GroupBaseDN: c.GroupBaseDN,
		GroupFilter: c.GroupFilter, GroupNameAttribute: c.GroupNameAttribute, NestedGroups: c.NestedGroups,
		Mapping: mappingFrom(c.Mapping()),
	}
}

// normalize trims what the user typed and drops empty list entries.
func (m *Mapping) normalize() {
	// Rows are kept even when empty, so errors name the row the user sees.
	rows := make([]GroupRole, 0, len(m.GroupRoles))
	for _, r := range m.GroupRoles {
		rows = append(rows, GroupRole{Group: strings.TrimSpace(r.Group), Roles: cleanList(r.Roles)})
	}
	m.GroupRoles = rows
	m.DefaultRoles = cleanList(m.DefaultRoles)
	m.AllowedGroups = cleanList(m.AllowedGroups)
}

func (o *OIDCSettings) normalize() {
	o.Preset = strings.TrimSpace(o.Preset)
	for _, p := range []*string{&o.IssuerURL, &o.ClientID, &o.ClientAuth, &o.DisplayName, &o.UsernameClaim, &o.EmailClaim, &o.NameClaim, &o.GroupsClaim} {
		*p = strings.TrimSpace(*p)
	}
	o.IssuerURL = strings.TrimSuffix(o.IssuerURL, "/")
	o.Scopes = cleanList(o.Scopes)
	o.CAPEM = strings.TrimSpace(o.CAPEM)
	o.Mapping.normalize()
}

func (l *LDAPSettings) normalize() {
	l.Preset = strings.TrimSpace(l.Preset)
	for _, p := range []*string{&l.URL, &l.BindDN, &l.UserBaseDN, &l.UserFilter, &l.UsernameAttribute, &l.EmailAttribute,
		&l.NameAttribute, &l.GroupAttribute, &l.GroupBaseDN, &l.GroupFilter, &l.GroupNameAttribute} {
		*p = strings.TrimSpace(*p)
	}
	l.CAPEM = strings.TrimSpace(l.CAPEM)
	l.Mapping.normalize()
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return slices.Clone(s)
}

// ---------------------------------------------------------------- storage

// loadDoc reads a settings row into v. found is false when it does not exist.
func (s *Service) loadDoc(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, key string, v any, lock bool,
) (meta Meta, found bool, err error) {
	sql := `SELECT value, updated_at, updated_by FROM settings WHERE key = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var (
		raw []byte
		at  time.Time
		by  string
	)
	err = q.QueryRow(ctx, sql, key).Scan(&raw, &at, &by)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Meta{Source: SourceDefault, FileFields: []string{}}, false, nil
	case err != nil:
		return Meta{}, false, fmt.Errorf("settings: read %s: %w", key, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return Meta{}, false, fmt.Errorf("settings: decode %s: %w", key, err)
	}
	at = at.UTC()
	return Meta{Source: SourceDatabase, FileFields: []string{}, UpdatedAt: &at, UpdatedBy: by}, true, nil
}

func saveDoc(ctx context.Context, tx pgx.Tx, key string, v any, by string) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO settings (key, value, updated_at, updated_by) VALUES ($1, $2, now(), $3)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by`,
		key, raw, by); err != nil {
		return fmt.Errorf("settings: save %s: %w", key, err)
	}
	return bumpRevision(ctx, tx)
}

func bumpRevision(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `UPDATE settings_revision SET revision = revision + 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("settings: bump revision: %w", err)
	}
	return nil
}

func (s *Service) currentRevision(ctx context.Context) (int64, error) {
	var rev int64
	if err := s.pool.QueryRow(ctx, `SELECT revision FROM settings_revision WHERE id = 1`).Scan(&rev); err != nil {
		return 0, fmt.Errorf("settings: read revision: %w", err)
	}
	return rev, nil
}

// ---------------------------------------------------------------- OIDC

// RedirectURI is the OIDC callback URL to register at the provider.
func (s *Service) RedirectURI() string {
	return strings.TrimSuffix(s.opts.BaseURL, "/") + oidc.CallbackPath
}

// GetOIDC returns the OIDC settings: the file's when it enables OIDC, else
// the stored ones, else the defaults.
func (s *Service) GetOIDC(ctx context.Context) (*OIDC, error) {
	if err := s.authorize(ctx, core.ActionRead, AuditRead, "settings:oidc"); err != nil {
		return nil, err
	}
	if f := s.opts.File.OIDC; f.Enabled {
		return &OIDC{OIDCSettings: oidcSettingsFrom(f), ClientSecretConfigured: f.ClientSecret != "", CAFile: f.CAFile,
			RedirectURI: s.RedirectURI(), Meta: Meta{Source: SourceFile, FileFields: oidcFields}}, nil
	}
	var doc oidcDoc
	meta, found, err := s.loadDoc(ctx, s.pool, keyOIDC, &doc, false)
	if err != nil {
		return nil, err
	}
	set := oidcSettingsFrom(doc.config(""))
	set.Preset = doc.Preset
	if !found {
		set = oidcSettingsFrom(auth.DefaultConfig().OIDC)
	}
	return &OIDC{OIDCSettings: set, ClientSecretConfigured: doc.ClientSecret != "", RedirectURI: s.RedirectURI(), Meta: meta}, nil
}

// PutOIDC validates and stores the OIDC settings and puts them into effect.
func (s *Service) PutOIDC(ctx context.Context, in OIDCInput) (*OIDC, error) {
	if err := s.authorize(ctx, core.ActionAdmin, AuditOIDCUpdate, "settings:oidc"); err != nil {
		return nil, err
	}
	if s.opts.File.OIDC.Enabled {
		return nil, fmt.Errorf("OIDC is set in the configuration file (auth.oidc / HANGAR_AUTH_OIDC_*); change it there: %w", core.ErrInvalid)
	}
	in.normalize()
	var changed []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var old oidcDoc
		if _, _, err := s.loadDoc(ctx, tx, keyOIDC, &old, true); err != nil {
			return err
		}
		secret, sealed, fe, err := s.mergeSecret(old.ClientSecret, in.ClientSecret, in.ClearClientSecret, "client_secret")
		if err != nil {
			return err
		}
		cfg := in.config(secret)
		s.problems(fe, cfg.Problems())
		checkPreset(fe, in.Preset)
		if err := s.checkMapping(ctx, fe, in.Mapping); err != nil {
			return err
		}
		if err := fe.err(); err != nil {
			return err
		}
		doc := oidcDoc{OIDCSettings: oidcSettingsFrom(cfg), ClientSecret: sealed}
		doc.Preset = in.Preset
		changed = diffFields(old, doc, old.ClientSecret != sealed, "client_secret")
		return saveDoc(ctx, tx, keyOIDC, doc, core.PrincipalFrom(ctx).Name)
	})
	if err != nil {
		return nil, err
	}
	s.record(ctx, core.AuditEvent{Action: AuditOIDCUpdate, Target: "settings:oidc",
		Details: map[string]any{"changed": changed, "enabled": in.Enabled}})
	if err := s.changed(ctx, keyOIDC); err != nil {
		return nil, fmt.Errorf("saved, but not applied: %w", err)
	}
	return s.GetOIDC(ctx)
}

// ---------------------------------------------------------------- LDAP

// GetLDAP returns the LDAP settings, like GetOIDC.
func (s *Service) GetLDAP(ctx context.Context) (*LDAP, error) {
	if err := s.authorize(ctx, core.ActionRead, AuditRead, "settings:ldap"); err != nil {
		return nil, err
	}
	if f := s.opts.File.LDAP; f.Enabled {
		return &LDAP{LDAPSettings: ldapSettingsFrom(f), BindPasswordConfigured: f.BindPassword != "", CAFile: f.CAFile,
			Meta: Meta{Source: SourceFile, FileFields: ldapFields}}, nil
	}
	var doc ldapDoc
	meta, found, err := s.loadDoc(ctx, s.pool, keyLDAP, &doc, false)
	if err != nil {
		return nil, err
	}
	set := ldapSettingsFrom(doc.config(""))
	set.Preset = doc.Preset
	if !found {
		set = ldapSettingsFrom(auth.DefaultConfig().LDAP)
	}
	return &LDAP{LDAPSettings: set, BindPasswordConfigured: doc.BindPassword != "", Meta: meta}, nil
}

// PutLDAP validates and stores the LDAP settings and puts them into effect.
func (s *Service) PutLDAP(ctx context.Context, in LDAPInput) (*LDAP, error) {
	if err := s.authorize(ctx, core.ActionAdmin, AuditLDAPUpdate, "settings:ldap"); err != nil {
		return nil, err
	}
	if s.opts.File.LDAP.Enabled {
		return nil, fmt.Errorf("LDAP is set in the configuration file (auth.ldap / HANGAR_AUTH_LDAP_*); change it there: %w", core.ErrInvalid)
	}
	in.normalize()
	var changed []string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var old ldapDoc
		if _, _, err := s.loadDoc(ctx, tx, keyLDAP, &old, true); err != nil {
			return err
		}
		password, sealed, fe, err := s.mergeSecret(old.BindPassword, in.BindPassword, in.ClearBindPassword, "bind_password")
		if err != nil {
			return err
		}
		cfg := in.config(password)
		s.problems(fe, cfg.Problems())
		checkPreset(fe, in.Preset)
		if err := s.checkMapping(ctx, fe, in.Mapping); err != nil {
			return err
		}
		if err := fe.err(); err != nil {
			return err
		}
		doc := ldapDoc{LDAPSettings: ldapSettingsFrom(cfg), BindPassword: sealed}
		doc.Preset = in.Preset
		changed = diffFields(old, doc, old.BindPassword != sealed, "bind_password")
		return saveDoc(ctx, tx, keyLDAP, doc, core.PrincipalFrom(ctx).Name)
	})
	if err != nil {
		return nil, err
	}
	s.record(ctx, core.AuditEvent{Action: AuditLDAPUpdate, Target: "settings:ldap",
		Details: map[string]any{"changed": changed, "enabled": in.Enabled}})
	if err := s.changed(ctx, keyLDAP); err != nil {
		return nil, fmt.Errorf("saved, but not applied: %w", err)
	}
	return s.GetLDAP(ctx)
}

// ---------------------------------------------------------------- shared

// mergeSecret decides the secret to store: a new value, the stored one
// (empty input), or none (clear). It returns the plaintext, the sealed form
// and FieldErrors to add problems to.
func (s *Service) mergeSecret(stored, given string, clear bool, field string) (plain, sealed string, fe FieldErrors, err error) {
	fe = FieldErrors{}
	switch {
	case clear:
		return "", "", fe, nil
	case given != "":
		if s.opts.Keyring == nil {
			fe.add(field, errNoKeyring)
			return given, "", fe, nil
		}
		sealed, err := s.seal(given)
		return given, sealed, fe, err
	case stored != "":
		plain, err := s.open(stored)
		if err != nil {
			fe.add(field, "the stored value cannot be opened ("+err.Error()+"); enter it again")
			return "", "", fe, nil
		}
		return plain, stored, fe, nil
	}
	return "", "", fe, nil
}

// presetPattern is the form of a preset name: a short lower-case slug.
var presetPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// checkPreset accepts an empty preset or a slug of at most 32 characters.
// The list of presets belongs to the web UI, so any slug is stored.
func checkPreset(fe FieldErrors, preset string) {
	if preset != "" && (len(preset) > 32 || !presetPattern.MatchString(preset)) {
		fe.add("preset", "must be a lower-case name such as keycloak (letters, digits and '-', at most 32 characters)")
	}
}

// problems adds the auth package's findings, except the mapping table,
// which checkMapping reports row by row.
func (s *Service) problems(fe FieldErrors, ps []auth.FieldError) {
	for _, p := range ps {
		if p.Field == "group_roles" {
			continue
		}
		fe.add(p.Field, p.Message)
	}
}

// checkMapping reports mapping problems by row ("group_roles[2].roles")
// and roles that do not exist.
func (s *Service) checkMapping(ctx context.Context, fe FieldErrors, m Mapping) error {
	if len(m.GroupRoles) > 1000 {
		fe.add("group_roles", "has more than 1000 rows")
	}
	var names []string
	seen := map[string]int{}
	for i, r := range m.GroupRoles {
		key := strings.ToLower(strings.TrimPrefix(r.Group, "/"))
		switch {
		case r.Group == "":
			fe.add(fmt.Sprintf("group_roles[%d].group", i), "is required")
		case seen[key] > 0:
			fe.add(fmt.Sprintf("group_roles[%d].group", i), fmt.Sprintf("is already mapped in row %d", seen[key]))
		default:
			seen[key] = i + 1
		}
		if len(r.Roles) == 0 {
			fe.add(fmt.Sprintf("group_roles[%d].roles", i), "pick at least one role")
		}
		for _, role := range r.Roles {
			if !auth.ValidRoleName(role) {
				fe.add(fmt.Sprintf("group_roles[%d].roles", i), fmt.Sprintf("%q is not a valid role name", role))
			}
		}
		names = append(names, r.Roles...)
	}
	for _, role := range m.DefaultRoles {
		if !auth.ValidRoleName(role) {
			fe.add("default_roles", fmt.Sprintf("%q is not a valid role name", role))
		}
	}
	missing, err := s.opts.Roles.MissingRoles(ctx, append(names, m.DefaultRoles...))
	if err != nil {
		return err
	}
	for _, role := range missing {
		for i, r := range m.GroupRoles {
			if slices.Contains(r.Roles, role) {
				fe.add(fmt.Sprintf("group_roles[%d].roles", i), fmt.Sprintf("role %q does not exist", role))
			}
		}
		if slices.Contains(m.DefaultRoles, role) {
			fe.add("default_roles", fmt.Sprintf("role %q does not exist", role))
		}
	}
	return nil
}

// diffFields names the top-level JSON fields that differ between two
// documents, plus secretField when secretChanged. Secret values never
// appear.
func diffFields(old, cur any, secretChanged bool, secretField string) []string {
	a, b := fieldsOf(old), fieldsOf(cur)
	out := []string{}
	for _, k := range slices.Sorted(maps.Keys(b)) {
		if k == secretField {
			continue
		}
		if string(a[k]) != string(b[k]) {
			out = append(out, k)
		}
	}
	if secretChanged {
		out = append(out, secretField)
	}
	return out
}

func fieldsOf(v any) map[string]json.RawMessage {
	raw, _ := json.Marshal(v)
	m := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// effectiveSignIn returns the OIDC and LDAP settings to apply: the file's
// for a provider the file enables, else the stored ones.
func (s *Service) effectiveSignIn(ctx context.Context) (auth.OIDCConfig, auth.LDAPConfig, error) {
	oidcCfg, ldapCfg := s.opts.File.OIDC.WithDefaults(), s.opts.File.LDAP.WithDefaults()
	if !oidcCfg.Enabled {
		var doc oidcDoc
		if _, found, err := s.loadDoc(ctx, s.pool, keyOIDC, &doc, false); err != nil {
			return oidcCfg, ldapCfg, err
		} else if found {
			secret, err := s.open(doc.ClientSecret)
			if err != nil {
				s.log.ErrorContext(ctx, "settings: the stored OIDC client secret cannot be opened", "error", err)
			}
			oidcCfg = doc.config(secret)
		}
	}
	if !ldapCfg.Enabled {
		var doc ldapDoc
		if _, found, err := s.loadDoc(ctx, s.pool, keyLDAP, &doc, false); err != nil {
			return oidcCfg, ldapCfg, err
		} else if found {
			password, err := s.open(doc.BindPassword)
			if err != nil {
				s.log.ErrorContext(ctx, "settings: the stored LDAP bind password cannot be opened", "error", err)
			}
			ldapCfg = doc.config(password)
		}
	}
	return oidcCfg, ldapCfg, nil
}
