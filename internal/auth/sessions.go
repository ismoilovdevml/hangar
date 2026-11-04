package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Session is a browser session. The cookie secret is never stored.
type Session struct {
	ID        string
	UserID    string
	CSRFToken string
	Remember  bool
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ValidCSRF reports whether token matches the session's CSRF token (constant time).
func (s *Session) ValidCSRF(token string) bool {
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.CSRFToken)) == 1
}

// sessionTouchInterval throttles last_seen_at updates.
const sessionTouchInterval = time.Minute

// CreateSession starts a browser session for p (a user who just signed in)
// and returns the cookie secret.
func (s *Service) CreateSession(ctx context.Context, p *core.Principal, remember bool) (string, *Session, error) {
	if p.IsAnonymous() || !validUUID(p.ID) {
		return "", nil, fmt.Errorf("create session: %w", core.ErrUnauthenticated)
	}
	secret, err := randomURLSafe(32)
	if err != nil {
		return "", nil, fmt.Errorf("create session: %w", err)
	}
	csrf, err := randomURLSafe(32)
	if err != nil {
		return "", nil, fmt.Errorf("create session: %w", err)
	}
	ttl := s.cfg.Session.TTL
	if remember {
		ttl = s.cfg.Session.RememberTTL
	}
	info := core.RequestInfoFrom(ctx)
	var ip *netip.Addr
	if a, err := netip.ParseAddr(info.RemoteIP); err == nil {
		ip = &a
	}
	now := s.now()
	sess := &Session{UserID: p.ID, CSRFToken: csrf, Remember: remember, ExpiresAt: now.Add(ttl).UTC().Truncate(time.Microsecond)}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO sessions (hash, user_id, csrf_token, remember, ip, user_agent, created_at, last_seen_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $8) RETURNING id::text, created_at`,
		hashSecret(secret), p.ID, csrf, remember, ip, truncate(info.UserAgent, 512), now, sess.ExpiresAt,
	).Scan(&sess.ID, &sess.CreatedAt)
	if err != nil {
		return "", nil, dbErr(err, "create session")
	}
	return secret, sess, nil
}

// SessionPrincipal resolves a session cookie to its user. Unknown, expired
// sessions and disabled users return ErrUnauthenticated.
func (s *Service) SessionPrincipal(ctx context.Context, secret string) (*core.Principal, *Session, error) {
	if secret == "" || len(secret) > 128 {
		return nil, nil, fmt.Errorf("session: %w", core.ErrUnauthenticated)
	}
	now := s.now()
	sess := &Session{}
	var lastSeen time.Time
	u, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT `+userCols+`, se.id::text, se.csrf_token, se.remember, se.created_at, se.expires_at, se.last_seen_at
		FROM sessions se JOIN users u ON u.id = se.user_id
		WHERE se.hash = $1 AND se.expires_at > $2 AND NOT u.disabled`, hashSecret(secret), now),
		&sess.ID, &sess.CSRFToken, &sess.Remember, &sess.CreatedAt, &sess.ExpiresAt, &lastSeen)
	if err != nil {
		if err = dbErr(err, "session"); errors.Is(err, core.ErrNotFound) {
			return nil, nil, fmt.Errorf("session expired or unknown: %w", core.ErrUnauthenticated)
		}
		return nil, nil, err
	}
	sess.UserID = u.ID
	if now.Sub(lastSeen) > sessionTouchInterval {
		if _, err := s.pool.Exec(ctx, `UPDATE sessions SET last_seen_at = $2 WHERE id = $1`, sess.ID, now); err != nil {
			s.log.WarnContext(ctx, "auth: touch session", "error", err)
		}
		s.noteUserActive(u.ID)
	}
	return u.principal(u.Source), sess, nil
}

// DeleteSession ends the session with this cookie secret (logout).
func (s *Service) DeleteSession(ctx context.Context, secret string) error {
	if secret == "" {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE hash = $1`, hashSecret(secret))
	return dbErr(err, "delete session")
}
