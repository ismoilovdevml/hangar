package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// LockedError is returned while an account is locked after repeated failed
// logins. It wraps core.ErrUnauthenticated, so package clients get a 401.
type LockedError struct {
	Until time.Time
}

func (e *LockedError) Error() string {
	return "account locked until " + e.Until.UTC().Format(time.RFC3339)
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
)

var errBadCredentials = fmt.Errorf("invalid credentials: %w", core.ErrUnauthenticated)

// Authenticate implements core.Authenticator: empty credentials give the
// anonymous principal; a token (Bearer, or as the Basic password) or a local
// username/password give the owner's principal with roles filled in. Wrong,
// expired or revoked credentials return ErrUnauthenticated and are audited;
// a locked account returns *LockedError.
func (s *Service) Authenticate(ctx context.Context, c core.Credentials) (*core.Principal, error) {
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
		return u.principal(SourceLocal), nil
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

// checkPassword is verifyPassword with the Basic-auth cache in front.
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
	u, err := s.verifyPassword(ctx, username, password, method)
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
	u, err := s.userByName(ctx, s.pool, username)
	if errors.Is(err, core.ErrNotFound) {
		s.equalizeTiming(ctx, password)
		fail("unknown_user", nil)
		return nil, errBadCredentials
	}
	if err != nil {
		return nil, err
	}
	if u.Kind != KindUser || u.Source != SourceLocal || u.passwordHash == "" {
		s.equalizeTiming(ctx, password)
		fail("no_local_password", nil)
		return nil, errBadCredentials
	}
	if u.Disabled {
		s.equalizeTiming(ctx, password)
		fail("disabled", nil)
		return nil, errBadCredentials
	}
	now := s.now()
	if u.lockedAt(now) {
		fail("locked", map[string]any{"locked_until": u.LockedUntil.UTC()})
		return nil, &LockedError{Until: *u.LockedUntil}
	}
	ok, rehash, err := s.hasher.Verify(ctx, u.passwordHash, password)
	if err != nil && !errors.Is(err, errBadHash) {
		return nil, fmt.Errorf("verify password: %w", err)
	}
	if err != nil {
		s.log.ErrorContext(ctx, "auth: stored password hash is malformed", "user", u.Username)
	}
	if !ok {
		attempts, lockedUntil, lerr := s.recordFailure(ctx, u.ID, now)
		if lerr != nil {
			return nil, lerr
		}
		details := map[string]any{"attempts": attempts}
		if lockedUntil != nil {
			details["locked_until"] = lockedUntil.UTC()
		}
		fail("bad_password", details)
		if lockedUntil != nil {
			s.basic.clear()
			return nil, &LockedError{Until: *lockedUntil}
		}
		return nil, errBadCredentials
	}
	if u.failedLogins > 0 || u.LockedUntil != nil {
		if _, err := s.pool.Exec(ctx, `
			UPDATE users SET failed_logins = 0, failed_window_start = NULL, locked_until = NULL
			WHERE id = $1`, u.ID); err != nil {
			return nil, fmt.Errorf("reset failed logins: %w", err)
		}
		u.failedLogins, u.LockedUntil = 0, nil
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

// recordFailure counts a failed password attempt in the lockout window and
// locks the account when the limit is reached. It returns the attempt count
// and, when this attempt locked the account, the lock expiry.
func (s *Service) recordFailure(ctx context.Context, userID string, now time.Time) (int, *time.Time, error) {
	limit := s.cfg.Lockout.MaxAttempts
	if limit <= 0 {
		limit = math.MaxInt32
	}
	var (
		attempts int
		locked   *time.Time
	)
	err := s.pool.QueryRow(ctx, `
		WITH c AS (
			SELECT id, CASE WHEN failed_window_start IS NULL OR failed_window_start <= $3
			                THEN 1 ELSE failed_logins + 1 END AS n
			FROM users WHERE id = $1 FOR UPDATE
		)
		UPDATE users u SET
			failed_logins = CASE WHEN c.n >= $4 THEN 0 ELSE c.n END,
			failed_window_start = CASE WHEN c.n >= $4 THEN NULL WHEN c.n = 1 THEN $2 ELSE u.failed_window_start END,
			locked_until = CASE WHEN c.n >= $4 THEN $5 ELSE u.locked_until END
		FROM c WHERE u.id = c.id
		RETURNING c.n, CASE WHEN c.n >= $4 THEN u.locked_until END`,
		userID, now, now.Add(-s.cfg.Lockout.Window), limit, now.Add(s.cfg.Lockout.Duration),
	).Scan(&attempts, &locked)
	if err != nil {
		return 0, nil, dbErr(err, "record failed login")
	}
	return attempts, locked, nil
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
	now := s.now()
	var (
		tokenID string
		scopes  []string
		claims  []byte
	)
	u, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT `+userCols+`, t.id::text, t.scopes, t.claims
		FROM tokens t
		JOIN users u ON u.id = t.user_id
		LEFT JOIN tokens pt ON pt.id = t.parent_id
		WHERE t.hash = $1 AND t.revoked_at IS NULL AND t.expires_at > $2 AND NOT u.disabled
		  AND (t.parent_id IS NULL OR (pt.revoked_at IS NULL AND pt.expires_at > $2))`,
		hashSecret(secret), now), &tokenID, &scopes, &claims)
	if err != nil {
		if err = dbErr(err, "token"); errors.Is(err, core.ErrNotFound) {
			return fail("unknown_expired_or_revoked")
		}
		return nil, err
	}
	p := u.principal(SourceToken)
	p.TokenID = tokenID
	if scopes != nil {
		p.Scopes = parsePrivileges(scopes)
	}
	if len(claims) > 0 && string(claims) != "{}" {
		if err := json.Unmarshal(claims, &p.Claims); err != nil {
			s.log.WarnContext(ctx, "auth: token claims unreadable", "token_id", tokenID, "error", err)
		}
	}
	s.noteTokenUse(tokenID, u.ID)
	return p, nil
}

// auditName bounds attacker-supplied usernames written to the audit log.
func auditName(s string) string {
	const maxLen = 128
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return strings.ToValidUTF8(s, "?")
}
