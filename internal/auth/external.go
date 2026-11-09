package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// External identity sources and the login methods recorded for them.
const (
	SourceOIDC = "oidc"
	SourceLDAP = "ldap"

	MethodOIDC = "oidc" // authorization code flow
	MethodLDAP = "ldap" // login form with realm "ldap"
)

// ExternalIdentity is a user as an external provider describes them after a
// successful sign-in.
type ExternalIdentity struct {
	// Source is SourceOIDC or SourceLDAP.
	Source string
	// Issuer and Subject identify the user at the provider (OIDC iss and
	// sub). An empty Subject links the identity by username instead.
	Issuer  string
	Subject string
	// Username is the Hangar username (it must pass ValidateUsername).
	Username    string
	Email       string
	DisplayName string
	// Groups the user belongs to, matched against the role mapping.
	Groups []string
}

// Directory authenticates a username and password against an LDAP
// directory. Wrong credentials and unknown users return an error wrapping
// core.ErrUnauthenticated; an unreachable or misconfigured directory returns
// any other error.
type Directory interface {
	Authenticate(ctx context.Context, username, password string) (*ExternalIdentity, error)
}

// ErrDirectoryUnavailable is returned when the LDAP directory cannot be
// reached or answers unexpectedly. It wraps core.ErrUpstream.
var ErrDirectoryUnavailable = fmt.Errorf("directory unavailable: %w", core.ErrUpstream)

// Codes of ExternalError, also used as the error query parameter of the
// login page after a failed single sign-on.
const (
	CodeAccountConflict = "account_conflict"
	CodeAccountDisabled = "account_disabled"
	CodeNotProvisioned  = "not_provisioned"
	CodeGroupDenied     = "group_denied"
	CodeInvalidProfile  = "invalid_profile"
)

// ExternalError explains why an external identity may not sign in.
type ExternalError struct {
	Code    string
	Message string
}

func (e *ExternalError) Error() string { return e.Message }

// Unwrap maps the code to a core sentinel for HTTP status mapping.
func (e *ExternalError) Unwrap() error {
	switch e.Code {
	case CodeAccountConflict:
		return core.ErrAlreadyExists
	case CodeAccountDisabled:
		return core.ErrUnauthenticated
	case CodeInvalidProfile:
		return core.ErrInvalid
	default:
		return core.ErrForbidden
	}
}

// RecordLoginFailure audits a failed sign-in that did not reach the service
// (for example an OIDC callback with a bad state).
func (s *Service) RecordLoginFailure(ctx context.Context, username, method, reason string) {
	s.record(ctx, core.AuditEvent{
		Actor: auditName(username), Action: AuditLoginFailed, Target: "user:" + auditName(username),
		Outcome: core.OutcomeDenied, Details: map[string]any{"method": method, "reason": reason},
	})
}

func (s *Service) mappingFor(source string) (RoleMapping, error) {
	ext := s.ext.Load()
	switch {
	case source == SourceOIDC && ext.oidc.Enabled:
		return ext.oidc.Mapping(), nil
	case source == SourceLDAP && ext.ldap.Enabled:
		return ext.ldap.Mapping(), nil
	}
	return RoleMapping{}, fmt.Errorf("sign-in with %q is not enabled: %w", source, core.ErrInvalid)
}

// SignInExternal admits an identity a provider has just authenticated: it
// finds the linked user (by issuer and subject, else by username) or creates
// one when the mapping allows, refreshes the profile, re-syncs the roles the
// group mapping grants (roles an administrator bound stay) and returns the
// principal. A username held by a local user, a service account or another
// external identity is never taken over. method is recorded in the audit
// log; MethodBasic sign-ins are not audited on success, like local Basic.
func (s *Service) SignInExternal(ctx context.Context, id ExternalIdentity, method string) (*core.Principal, error) {
	m, err := s.mappingFor(id.Source)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*core.Principal, error) {
		reason := "internal"
		var xe *ExternalError
		if errors.As(err, &xe) {
			reason = xe.Code
		}
		s.record(ctx, core.AuditEvent{
			Actor: auditName(id.Username), Action: AuditLoginFailed, Target: "user:" + auditName(id.Username),
			Outcome: core.OutcomeDenied, Details: map[string]any{"method": method, "reason": reason, "source": id.Source},
		})
		return nil, err
	}
	if err := ValidateUsername(id.Username); err != nil {
		return fail(&ExternalError{Code: CodeInvalidProfile,
			Message: fmt.Sprintf("the %s account has no usable username (%q): %v", id.Source, id.Username, err)})
	}
	if !m.Allows(id.Groups) {
		return fail(&ExternalError{Code: CodeGroupDenied, Message: "your groups are not allowed to sign in to Hangar"})
	}
	want := m.Roles(id.Groups)
	if len(id.Groups) > 0 {
		dbRoles, err := s.RolesForGroups(ctx, id.Source, GroupVariants(id.Groups))
		if err != nil {
			return fail(err)
		}
		want = slices.Compact(slices.Sorted(slices.Values(append(want, dbRoles...))))
	}
	var (
		uid            string
		created        bool
		added, removed []string
	)
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if uid, created, err = s.linkExternal(ctx, tx, &id, m.AutoCreate); err != nil {
			return err
		}
		added, removed, err = s.syncMappedRoles(ctx, tx, uid, id.Source, want)
		return err
	})
	if err != nil {
		return fail(err)
	}
	if created || len(added) > 0 || len(removed) > 0 {
		s.credentialsChanged()
	}
	u, err := s.userByID(ctx, s.pool, uid)
	if err != nil {
		return nil, err
	}
	p := u.principal(u.Source)
	actx := core.WithPrincipal(ctx, p)
	if created {
		s.record(actx, core.AuditEvent{
			Action: AuditUserCreate, Target: "user:" + u.Username,
			Details: map[string]any{"id": u.ID, "kind": u.Kind, "source": u.Source, "roles": nonNil(u.Roles), "provisioned": true},
		})
	}
	if method != MethodBasic || len(added) > 0 || len(removed) > 0 {
		s.record(actx, core.AuditEvent{
			Action: AuditLogin, Target: "user:" + u.Username,
			Details: map[string]any{"method": method, "roles_added": nonNil(added), "roles_removed": nonNil(removed)},
		})
	}
	s.noteUserActive(u.ID)
	return p, nil
}

// linkExternal finds or creates the user for id inside tx and updates its
// profile. It returns the user id and whether the user was created.
func (s *Service) linkExternal(ctx context.Context, tx pgx.Tx, id *ExternalIdentity, autoCreate bool) (string, bool, error) {
	var (
		uid      string
		bySub    bool
		disabled bool
		oldName  string
	)
	if id.Subject != "" {
		err := tx.QueryRow(ctx, `
			SELECT id::text, username, disabled FROM users
			WHERE source = $1 AND external_issuer = $2 AND external_id = $3 FOR UPDATE`,
			id.Source, id.Issuer, id.Subject).Scan(&uid, &oldName, &disabled)
		switch {
		case err == nil:
			bySub = true
		case !errors.Is(err, pgx.ErrNoRows):
			return "", false, dbErr(err, "find external user")
		}
	}
	if !bySub {
		var (
			source, kind string
			issuer, sub  *string
		)
		err := tx.QueryRow(ctx, `
			SELECT id::text, source, kind, external_issuer, external_id, disabled FROM users
			WHERE username = $1 FOR UPDATE`, id.Username).Scan(&uid, &source, &kind, &issuer, &sub, &disabled)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if !autoCreate {
				return "", false, &ExternalError{Code: CodeNotProvisioned,
					Message: fmt.Sprintf("there is no Hangar account for %q; ask an administrator", id.Username)}
			}
			uid, err := s.createExternal(ctx, tx, id)
			return uid, err == nil, err
		case err != nil:
			return "", false, dbErr(err, "find user")
		}
		oldName = id.Username
		conflict := source != id.Source || kind != KindUser ||
			(id.Subject != "" && sub != nil && (*sub != id.Subject || deref(issuer) != id.Issuer))
		if conflict {
			s.log.WarnContext(ctx, "auth: external sign-in collides with an existing user",
				"username", id.Username, "source", id.Source, "existing_source", source, "existing_kind", kind)
			return "", false, &ExternalError{Code: CodeAccountConflict,
				Message: fmt.Sprintf("the username %q already belongs to another Hangar account; ask an administrator", id.Username)}
		}
	}
	if disabled {
		return "", false, &ExternalError{Code: CodeAccountDisabled, Message: "this account is disabled"}
	}
	name := oldName
	if bySub && id.Username != oldName {
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE username = $1)`, id.Username).Scan(&taken); err != nil {
			return "", false, dbErr(err, "check username")
		}
		if taken {
			s.log.WarnContext(ctx, "auth: external user was renamed to a username already in use; keeping the old name",
				"username", oldName, "new_username", id.Username, "source", id.Source)
		} else {
			name = id.Username
			if _, err := endSessions(ctx, tx, uid, ""); err != nil {
				return "", false, err
			}
		}
	}
	email, err := s.freeEmail(ctx, tx, id, uid)
	if err != nil {
		return "", false, err
	}
	_, err = tx.Exec(ctx, `
		UPDATE users SET username = $2, email = $3, display_name = $4,
			external_issuer = coalesce($5, external_issuer), external_id = coalesce($6, external_id),
			updated_at = CASE WHEN username <> $2 OR email IS DISTINCT FROM $3 OR display_name <> $4 THEN now() ELSE updated_at END
		WHERE id = $1`, uid, name, email, profileName(id.DisplayName), nullIfEmpty(&id.Issuer), nullIfEmpty(&id.Subject))
	return uid, false, dbErr(err, "update external user")
}

func (s *Service) createExternal(ctx context.Context, tx pgx.Tx, id *ExternalIdentity) (string, error) {
	email, err := s.freeEmail(ctx, tx, id, "")
	if err != nil {
		return "", err
	}
	var uid string
	err = tx.QueryRow(ctx, `
		INSERT INTO users (username, kind, email, display_name, source, external_issuer, external_id)
		VALUES ($1, 'user', $2, $3, $4, $5, $6) RETURNING id::text`,
		id.Username, email, profileName(id.DisplayName), id.Source, nullIfEmpty(&id.Issuer), nullIfEmpty(&id.Subject)).Scan(&uid)
	return uid, dbErr(err, fmt.Sprintf("create user %q", id.Username))
}

// freeEmail returns the identity's email when it is valid and no other user
// has it (emails are unique), else nil.
func (s *Service) freeEmail(ctx context.Context, tx pgx.Tx, id *ExternalIdentity, uid string) (*string, error) {
	email := strings.TrimSpace(id.Email)
	if !validEmail(email) {
		return nil, nil
	}
	var taken bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = lower($1) AND ($2 = '' OR id::text <> $2))`,
		email, uid).Scan(&taken); err != nil {
		return nil, dbErr(err, "check email")
	}
	if taken {
		s.log.WarnContext(ctx, "auth: email of external user belongs to another user; not stored", "username", id.Username)
		return nil, nil
	}
	return &email, nil
}

func validEmail(email string) bool {
	return email != "" && validateProfile(nil, &email) == nil
}

func profileName(s string) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}

// syncMappedRoles makes the bindings that source's group mapping granted
// equal want. Bindings an administrator made are neither removed nor taken
// over; a wanted role the user already holds manually stays manual. Roles
// that no longer exist are skipped.
func (s *Service) syncMappedRoles(ctx context.Context, tx pgx.Tx, uid, source string, want []string) (added, removed []string, err error) {
	rows, err := tx.Query(ctx, `
		SELECT r.name, b.mapped_from FROM role_bindings b JOIN roles r ON r.id = b.role_id
		WHERE b.user_id = $1 AND b.subject_kind = 'user'`, uid)
	if err != nil {
		return nil, nil, dbErr(err, "load role bindings")
	}
	current := map[string]*string{}
	for rows.Next() {
		var name string
		var from *string
		if err := rows.Scan(&name, &from); err != nil {
			rows.Close()
			return nil, nil, err
		}
		current[name] = from
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var missing []string
	for _, r := range want {
		if _, ok := current[r]; !ok {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		rows, err := tx.Query(ctx, `
			INSERT INTO role_bindings (role_id, subject_kind, user_id, mapped_from)
			SELECT r.id, 'user', $1, $2 FROM roles r WHERE r.name = ANY($3)
			ON CONFLICT DO NOTHING
			RETURNING (SELECT name FROM roles WHERE id = role_id)`, uid, source, missing)
		if err != nil {
			return nil, nil, dbErr(err, "grant mapped roles")
		}
		if added, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return nil, nil, dbErr(err, "grant mapped roles")
		}
		slices.Sort(added)
		if len(added) < len(missing) {
			s.log.WarnContext(ctx, "auth: group mapping names roles that do not exist", "roles", missing, "granted", added)
		}
	}
	var drop []string
	for name, from := range current {
		if from != nil && *from == source && !slices.Contains(want, name) {
			drop = append(drop, name)
		}
	}
	if len(drop) > 0 {
		slices.Sort(drop)
		if _, err := tx.Exec(ctx, `
			DELETE FROM role_bindings b USING roles r
			WHERE r.id = b.role_id AND b.user_id = $1 AND b.subject_kind = 'user' AND b.mapped_from = $2 AND r.name = ANY($3)`,
			uid, source, drop); err != nil {
			return nil, nil, dbErr(err, "revoke mapped roles")
		}
		removed = drop
	}
	return added, removed, nil
}

// LoginLDAP verifies a username and password against the directory for an
// interactive sign-in (realm "ldap"), with the lockout of local accounts
// applied to users who signed in before.
func (s *Service) LoginLDAP(ctx context.Context, username, password string) (*core.Principal, error) {
	u, err := s.verifyLDAP(ctx, username, password, MethodLDAP)
	if err != nil {
		return nil, err
	}
	return u.principal(u.Source), nil
}

// verifyLDAP authenticates against the directory and admits the identity.
func (s *Service) verifyLDAP(ctx context.Context, username, password, method string) (*User, error) {
	dir := s.Directory()
	if dir == nil {
		return nil, fmt.Errorf("LDAP sign-in is not enabled: %w", core.ErrInvalid)
	}
	fail := func(reason string, details map[string]any) {
		if details == nil {
			details = map[string]any{}
		}
		details["method"], details["reason"], details["source"] = method, reason, SourceLDAP
		s.record(ctx, core.AuditEvent{
			Actor: auditName(username), Action: AuditLoginFailed, Target: "user:" + auditName(username),
			Outcome: core.OutcomeDenied, Details: details,
		})
	}
	// An empty password would be an unauthenticated bind, which many
	// directories accept for any DN.
	if username == "" || password == "" || len(username) > 256 || len(password) > 4096 {
		fail("malformed", nil)
		return nil, errBadCredentials
	}
	known, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users u
		WHERE lower(u.username) = lower($1) AND u.source = 'ldap' ORDER BY u.username = $1 DESC LIMIT 1`, username))
	if err = dbErr(err, "user"); err != nil && !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	if err != nil {
		known = nil
	}
	// The lockout is per (name, address) for names the directory knows and
	// names it does not alike (see Lockout).
	if err := s.signInLocked(ctx, username); err != nil {
		var locked *LockedError
		if errors.As(err, &locked) {
			fail("locked", map[string]any{"locked_until": locked.Until.UTC()})
		}
		return nil, err
	}
	if known != nil && known.Disabled {
		fail("disabled", nil)
		return nil, errBadCredentials
	}
	if err := s.waitSignIn(ctx, username); err != nil {
		return nil, err
	}
	id, err := dir.Authenticate(ctx, username, password)
	if err != nil {
		if !errors.Is(err, core.ErrUnauthenticated) {
			s.log.WarnContext(ctx, "auth: LDAP directory error", "error", err)
			fail("directory_error", nil)
			return nil, fmt.Errorf("%w: %w", ErrDirectoryUnavailable, err)
		}
		knownID, reason := "", "bad_credentials"
		if known != nil {
			knownID, reason = known.ID, "bad_password"
		}
		attempts, lockedUntil, lerr := s.noteSignInFailure(ctx, username, knownID)
		if lerr != nil {
			return nil, lerr
		}
		details := map[string]any{"attempts": attempts}
		if lockedUntil != nil {
			details["locked_until"] = lockedUntil.UTC()
		}
		fail(reason, details)
		if lockedUntil != nil {
			return nil, &LockedError{Until: *lockedUntil}
		}
		return nil, errBadCredentials
	}
	id.Source = SourceLDAP
	p, err := s.SignInExternal(ctx, *id, method)
	if err != nil {
		return nil, err
	}
	if known != nil && known.ID == p.ID {
		if err := s.noteSignInSuccess(ctx, known); err != nil {
			return nil, err
		}
	}
	return s.userByID(ctx, s.pool, p.ID)
}

// useLDAP reports whether Basic credentials for username go to the
// directory: LDAP is on and the name is not a local or other account.
func (s *Service) useLDAP(ctx context.Context, username string) (bool, error) {
	if s.Directory() == nil {
		return false, nil
	}
	var source string
	err := s.pool.QueryRow(ctx, `SELECT source FROM users WHERE username = $1`, username).Scan(&source)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return true, nil
	case err != nil:
		return false, dbErr(err, "user")
	}
	return source == SourceLDAP, nil
}

// external is a set of sign-in providers in effect.
type external struct {
	oidc OIDCConfig
	ldap LDAPConfig
	dir  Directory // nil when LDAP is off
}

// prepareExternal checks that every role named by an enabled provider's
// mapping exists and registers the providers as identity_providers rows, so
// roles can also map their groups (RoleInput.GroupMappings). A missing role
// is an error when strict (start-up configuration); otherwise it is logged
// and sign-ins skip it.
func (s *Service) prepareExternal(ctx context.Context, ext *external, strict bool) error {
	type provider struct {
		name    string
		enabled bool
		mapping RoleMapping
	}
	var errs []error
	for _, p := range []provider{
		{SourceOIDC, ext.oidc.Enabled, ext.oidc.Mapping()},
		{SourceLDAP, ext.ldap.Enabled, ext.ldap.Mapping()},
	} {
		if !p.enabled {
			continue
		}
		missing, err := s.MissingRoles(ctx, p.mapping.RoleNames())
		if err != nil {
			return err
		}
		for _, n := range missing {
			errs = append(errs, fmt.Errorf("auth.%s: role %q does not exist (create it first or fix group_roles/default_roles)", p.name, n))
		}
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO identity_providers (name, kind) VALUES ($1, $1)
			ON CONFLICT (name) DO UPDATE SET enabled = true, updated_at = now() WHERE NOT identity_providers.enabled`,
			p.name); err != nil {
			return dbErr(err, "register identity provider")
		}
	}
	if len(errs) == 0 {
		return nil
	}
	if !strict {
		s.log.WarnContext(ctx, "auth: sign-in role mapping names roles that do not exist; they are skipped", "error", errors.Join(errs...))
		return nil
	}
	return fmt.Errorf("%w: %w", errors.Join(errs...), core.ErrInvalid)
}

// MissingRoles returns the names among names that are not roles.
func (s *Service) MissingRoles(ctx context.Context, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT name FROM roles WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, dbErr(err, "load roles")
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, dbErr(err, "load roles")
	}
	var missing []string
	for _, n := range names {
		if !slices.Contains(found, n) && !slices.Contains(missing, n) {
			missing = append(missing, n)
		}
	}
	return missing, nil
}

// SetExternal replaces the sign-in providers in effect, for settings changed
// while the server runs. Defaults are applied to both configurations; dir
// is required when ldap is enabled and ignored otherwise. Roles the mappings
// name that do not exist are logged and skipped at sign-in. Cached Basic
// credentials are dropped, so a directory that was turned off stops
// admitting users at once.
func (s *Service) SetExternal(ctx context.Context, oidcCfg OIDCConfig, ldapCfg LDAPConfig, dir Directory) error {
	oidcCfg, ldapCfg = oidcCfg.WithDefaults(), ldapCfg.WithDefaults()
	if errs := append(oidcCfg.validate(), ldapCfg.validate()...); len(errs) > 0 {
		return fmt.Errorf("auth: %w: %w", errors.Join(errs...), core.ErrInvalid)
	}
	if !ldapCfg.Enabled {
		dir = nil
	} else if dir == nil {
		return fmt.Errorf("auth: LDAP is enabled but no directory is configured: %w", core.ErrInvalid)
	}
	ext := &external{oidc: oidcCfg, ldap: ldapCfg, dir: dir}
	s.extMu.Lock()
	defer s.extMu.Unlock()
	if err := s.prepareExternal(ctx, ext, false); err != nil {
		return err
	}
	s.ext.Store(ext)
	s.credentialsChanged()
	return nil
}

// External returns the OIDC and LDAP settings in effect.
func (s *Service) External() (OIDCConfig, LDAPConfig) {
	ext := s.ext.Load()
	return ext.oidc, ext.ldap
}

// Directory returns the LDAP directory in effect (nil when LDAP is off).
func (s *Service) Directory() Directory {
	ext := s.ext.Load()
	if !ext.ldap.Enabled {
		return nil
	}
	return ext.dir
}
