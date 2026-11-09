package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// LockedError is returned while a username is locked out for the client's
// address after repeated failed sign-ins (see Lockout); it is the same
// whether or not the username belongs to an account. It wraps
// core.ErrUnauthenticated, so package clients get a 401.
type LockedError struct {
	Until time.Time
}

func (e *LockedError) Error() string {
	return "too many failed sign-ins; locked until " + e.Until.UTC().Format(time.RFC3339)
}

// Unwrap makes errors.Is(err, core.ErrUnauthenticated) true.
func (e *LockedError) Unwrap() error { return core.ErrUnauthenticated }

// RetryAfter is the time left on the lock, rounded up to a second.
func (e *LockedError) RetryAfter(now time.Time) time.Duration {
	d := e.Until.Sub(now)
	if d < time.Second {
		return time.Second
	}
	return d.Round(time.Second)
}

// Login methods recorded in audit details.
const (
	MethodLogin = "login" // interactive /auth/login
	MethodBasic = "basic" // HTTP Basic on API or package requests
	MethodToken = "token"
	MethodRenew = "renew" // registry token renewal (ReloadPrincipal)
)

var errBadCredentials = fmt.Errorf("invalid credentials: %w", core.ErrUnauthenticated)

// Authenticate implements core.Authenticator: empty credentials give the
// anonymous principal; a token (Bearer, or as the Basic password) or a local
// username/password give the owner's principal with roles filled in. Wrong,
// expired or revoked credentials return ErrUnauthenticated and are audited;
// a locked account returns *LockedError. A correct password of a user who
// must change it returns ErrPasswordChangeRequired: package clients cannot
// use it until it is replaced.
func (s *Service) Authenticate(ctx context.Context, c core.Credentials) (*core.Principal, error) {
	p, err := s.AuthenticateRequest(ctx, c)
	if err == nil && p.PasswordChangeRequired {
		return nil, ErrPasswordChangeRequired
	}
	return p, err
}

// AuthenticateRequest is Authenticate for the REST API: a user who must
// change their password gets a principal with PasswordChangeRequired set
// instead of an error, so the API can let it reach the password change and
// nothing else.
func (s *Service) AuthenticateRequest(ctx context.Context, c core.Credentials) (*core.Principal, error) {
	switch {
	case c.Empty():
		return core.Anonymous(), nil
	case c.Token != "":
		return s.authenticateToken(ctx, c.Token)
	case strings.HasPrefix(c.Password, core.TokenPrefix) && wellFormedToken(c.Password):
		// Clients put the token in the password and anything in the
		// username (docker login, npm, pip); the secret alone identifies it.
		return s.authenticateToken(ctx, c.Password)
	default:
		u, err := s.checkPassword(ctx, c.Username, c.Password, MethodBasic)
		if err != nil {
			return nil, err
		}
		return u.principal(u.Source), nil
	}
}

// Login verifies a username and password for an interactive sign-in and
// audits the outcome. It never uses the Basic-auth cache.
func (s *Service) Login(ctx context.Context, username, password string) (*core.Principal, error) {
	u, err := s.verifyPassword(ctx, username, password, MethodLogin)
	if err != nil {
		return nil, err
	}
	p := u.principal(SourceLocal)
	s.record(core.WithPrincipal(ctx, p), core.AuditEvent{
		Action: AuditLogin, Target: "user:" + u.Username, Details: map[string]any{"method": MethodLogin},
	})
	return p, nil
}

// checkPassword is verifyPassword, or verifyLDAP for directory users, with
// the Basic-auth cache in front.
func (s *Service) checkPassword(ctx context.Context, username, password, method string) (*User, error) {
	key := s.basicKeyFor(username, password)
	if id, ok := s.basic.get(key); ok {
		u, err := s.userByID(ctx, s.pool, id)
		if err == nil && u.Username == username && !u.Disabled && !u.lockedAt(s.now()) {
			s.noteUserActive(u.ID)
			return u, nil
		}
		s.basic.del(key) // stale; fall through to a full check
	}
	ldap, err := s.useLDAP(ctx, username)
	if err != nil {
		return nil, err
	}
	var u *User
	if ldap {
		u, err = s.verifyLDAP(ctx, username, password, method)
	} else {
		u, err = s.verifyPassword(ctx, username, password, method)
	}
	if err != nil {
		return nil, err
	}
	s.basic.set(key, u.ID)
	return u, nil
}

// verifyPassword checks a local user's password with lockout accounting.
func (s *Service) verifyPassword(ctx context.Context, username, password, method string) (*User, error) {
	fail := func(reason string, details map[string]any) {
		if details == nil {
			details = map[string]any{}
		}
		details["method"], details["reason"] = method, reason
		s.record(ctx, core.AuditEvent{
			Actor: auditName(username), Action: AuditLoginFailed, Target: "user:" + auditName(username),
			Outcome: core.OutcomeDenied, Details: details,
		})
	}
	if username == "" || len(username) > 256 || len(password) > 4096 {
		fail("malformed", nil)
		return nil, errBadCredentials
	}
	// Answered the same way for every name, existing or not, and before
	// any password hash is computed.
	if err := s.signInLocked(ctx, username); err != nil {
		var locked *LockedError
		if errors.As(err, &locked) {
			fail("locked", map[string]any{"locked_until": locked.Until.UTC()})
		}
		return nil, err
	}
	if err := s.waitSignIn(ctx, username); err != nil {
		return nil, err
	}
	// refuse spends a verification's time on a name that cannot sign in
	// with a password, and counts it like a wrong password.
	refuse := func(reason, userID string) (*User, error) {
		s.equalizeTiming(ctx, password)
		attempts, lockedUntil, err := s.noteSignInFailure(ctx, username, userID)
		if err != nil {
			return nil, err
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
	u, err := s.userByName(ctx, s.pool, username)
	if errors.Is(err, core.ErrNotFound) {
		return refuse("unknown_user", "")
	}
	if err != nil {
		return nil, err
	}
	if u.Kind != KindUser || u.Source != SourceLocal || u.passwordHash == "" {
		return refuse("no_local_password", "")
	}
	if u.Disabled {
		return refuse("disabled", "")
	}
	ok, rehash, err := s.hasher.Verify(ctx, u.passwordHash, password)
	if err != nil && !errors.Is(err, errBadHash) {
		return nil, fmt.Errorf("verify password: %w", err)
	}
	if err != nil {
		s.log.ErrorContext(ctx, "auth: stored password hash is malformed", "user", u.Username)
	}
	if !ok {
		attempts, lockedUntil, lerr := s.noteSignInFailure(ctx, username, u.ID)
		if lerr != nil {
			return nil, lerr
		}
		details := map[string]any{"attempts": attempts}
		if lockedUntil != nil {
			details["locked_until"] = lockedUntil.UTC()
		}
		fail("bad_password", details)
		if lockedUntil != nil {
			return nil, &LockedError{Until: *lockedUntil}
		}
		return nil, errBadCredentials
	}
	if err := s.noteSignInSuccess(ctx, u); err != nil {
		return nil, err
	}
	if rehash {
		s.rehash(ctx, u, password)
	}
	s.noteUserActive(u.ID)
	return u, nil
}

// equalizeTiming spends the cost of one verification so unknown and
// disabled accounts answer as slowly as wrong passwords.
func (s *Service) equalizeTiming(ctx context.Context, password string) {
	_, _, _ = s.hasher.Verify(ctx, s.dummyHash, password)
}

// rehash upgrades a hash made with old parameters; failures only log.
func (s *Service) rehash(ctx context.Context, u *User, password string) {
	h, err := s.hasher.Hash(ctx, password)
	if err == nil {
		_, err = s.pool.Exec(ctx, `UPDATE users SET password_hash = $1 WHERE id = $2 AND password_hash = $3`, h, u.ID, u.passwordHash)
	}
	if err != nil {
		s.log.WarnContext(ctx, "auth: password rehash failed", "user", u.Username, "error", err)
	}
}

// authenticateToken resolves a personal, service-account or session token.
func (s *Service) authenticateToken(ctx context.Context, secret string) (*core.Principal, error) {
	fail := func(reason string) (*core.Principal, error) {
		prefix := secret
		if len(prefix) > tokenDisplayLen {
			prefix = prefix[:tokenDisplayLen]
		}
		if !strings.HasPrefix(prefix, core.TokenPrefix) {
			prefix = ""
		}
		s.record(ctx, core.AuditEvent{
			Action: AuditTokenAuthFailed, Target: "token:" + prefix, Outcome: core.OutcomeDenied,
			Details: map[string]any{"method": MethodToken, "reason": reason},
		})
		return nil, errBadCredentials
	}
	if !wellFormedToken(secret) {
		return fail("malformed")
	}
	p, reason, err := s.tokenPrincipal(ctx, `t.hash = $1`, hashSecret(secret))
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return fail(reason)
	}
	return p, nil
}

// tokenPrincipal loads the principal of the live token matching cond ($1 is
// arg) whose owner is enabled. A refusal returns a reason and no principal.
func (s *Service) tokenPrincipal(ctx context.Context, cond string, arg any) (*core.Principal, string, error) {
	now := s.now()
	var (
		tokenID  string
		scopes   []string
		claims   []byte
		readOnly bool
		cidrs    []string
	)
	// Session tokens minted from a restricted token keep its restrictions.
	u, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT `+userCols+`, t.id::text, t.scopes, t.claims,
			t.read_only OR coalesce(pt.read_only, false), coalesce(t.allowed_cidrs, pt.allowed_cidrs)::text[]
		FROM tokens t
		JOIN users u ON u.id = t.user_id
		LEFT JOIN tokens pt ON pt.id = t.parent_id
		WHERE `+cond+` AND t.revoked_at IS NULL AND t.expires_at > $2 AND NOT u.disabled
		  AND (t.parent_id IS NULL OR (pt.revoked_at IS NULL AND pt.expires_at > $2))`,
		arg, now), &tokenID, &scopes, &claims, &readOnly, &cidrs)
	if err != nil {
		if err = dbErr(err, "token"); errors.Is(err, core.ErrNotFound) {
			return nil, "unknown_expired_or_revoked", nil
		}
		return nil, "", err
	}
	if cidrs != nil && !addressAllowed(cidrs, core.RequestInfoFrom(ctx).RemoteIP) {
		return nil, "address_not_allowed", nil
	}
	p := u.principal(SourceToken)
	p.TokenID = tokenID
	if scopes != nil {
		p.Scopes = parsePrivileges(scopes)
	}
	if readOnly {
		p.ReadOnly = true
		p.Scopes = readOnlyScopes(p.Scopes)
	}
	if len(claims) > 0 && string(claims) != "{}" {
		if err := json.Unmarshal(claims, &p.Claims); err != nil {
			s.log.WarnContext(ctx, "auth: token claims unreadable", "token_id", tokenID, "error", err)
		}
	}
	s.noteTokenUse(tokenID, u.ID, core.RequestInfoFrom(ctx).RemoteIP)
	return p, "", nil
}

// ReloadPrincipal rebuilds a principal that authenticated earlier from the
// database, as it would authenticate now: the token it used must still be
// live (not revoked or expired, its address restrictions met), the account
// must exist and be enabled and must not be waiting for a password change,
// and roles are the current ones. The registry token service calls it
// before renewing a registry token, so disabling a user, revoking a token or
// removing a role ends registry access at the latest when the current token
// expires. A refusal is audited and returns ErrUnauthenticated.
func (s *Service) ReloadPrincipal(ctx context.Context, p *core.Principal) (*core.Principal, error) {
	if p.IsAnonymous() {
		return core.Anonymous(), nil
	}
	fail := func(reason string) (*core.Principal, error) {
		s.record(ctx, core.AuditEvent{
			Actor: auditName(p.Name), Action: AuditTokenAuthFailed, Target: "user:" + auditName(p.Name),
			Outcome: core.OutcomeDenied, Details: map[string]any{"method": MethodRenew, "reason": reason},
		})
		return nil, errBadCredentials
	}
	if !validUUID(p.ID) {
		return fail("unknown_user")
	}
	if p.TokenID != "" {
		if !validUUID(p.TokenID) {
			return fail("unknown_expired_or_revoked")
		}
		np, reason, err := s.tokenPrincipal(ctx, `t.id = $1`, p.TokenID)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return fail(reason)
		}
		if np.ID != p.ID {
			return fail("token_owner_changed")
		}
		return np, nil
	}
	u, err := s.userByID(ctx, s.pool, p.ID)
	if errors.Is(err, core.ErrNotFound) {
		return fail("unknown_user")
	}
	if err != nil {
		return nil, err
	}
	switch {
	case u.Disabled:
		return fail("disabled")
	case u.MustChangePassword:
		return fail("password_change_required")
	}
	source := p.Source
	if source == "" || source == SourceToken {
		source = u.Source
	}
	return u.principal(source), nil
}

// auditName bounds attacker-supplied usernames written to the audit log.
func auditName(s string) string {
	const maxLen = 128
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return strings.ToValidUTF8(s, "?")
}
