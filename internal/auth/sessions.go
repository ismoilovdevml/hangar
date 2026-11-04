package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"

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

// SessionInfo describes a browser session for the session lists. The secret
// and CSRF token are never exposed.
type SessionInfo struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	IP         *string   `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	Remember   bool      `json:"remember"`
	// Current marks the session making the request.
	Current bool `json:"current"`
}

// maxSessionsListed bounds a session list; older sessions beyond it are
// still ended by the "end all" actions.
const maxSessionsListed = 500

// authorizeUserSessions lets users manage their own sessions and holders of
// system:users:<action> anyone's. It returns the target's username.
func (s *Service) authorizeUserSessions(ctx context.Context, userID, auditAction string, action core.Action) (string, error) {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() {
		return "", fmt.Errorf("sessions: %w", core.ErrUnauthenticated)
	}
	if userID != p.ID {
		if err := s.require(ctx, auditAction, action, core.SystemResource(core.SystemUsers)); err != nil {
			return "", err
		}
	}
	u, err := s.userByID(ctx, s.pool, userID)
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// ListSessions lists the active browser sessions of a user, most recently
// used first: the caller's own, or anyone's with system:users:read.
// currentID marks the caller's session.
func (s *Service) ListSessions(ctx context.Context, userID, currentID string) ([]SessionInfo, error) {
	if _, err := s.authorizeUserSessions(ctx, userID, "session.list", core.ActionRead); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, created_at, last_seen_at, expires_at, host(ip), user_agent, remember
		FROM sessions WHERE user_id = $1 AND expires_at > $2
		ORDER BY last_seen_at DESC, id LIMIT $3`, userID, s.now(), maxSessionsListed)
	if err != nil {
		return nil, dbErr(err, "list sessions")
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SessionInfo, error) {
		var si SessionInfo
		err := r.Scan(&si.ID, &si.CreatedAt, &si.LastSeenAt, &si.ExpiresAt, &si.IP, &si.UserAgent, &si.Remember)
		si.Current = si.ID == currentID
		return si, err
	})
	if err != nil {
		return nil, dbErr(err, "list sessions")
	}
	return out, nil
}

// RevokeSession ends one browser session of a user: the caller's own, or
// anyone's with system:users:write. Unknown ids are not found.
func (s *Service) RevokeSession(ctx context.Context, userID, sessionID string) error {
	username, err := s.authorizeUserSessions(ctx, userID, AuditSessionRevoke, core.ActionWrite)
	if err != nil {
		return err
	}
	if !validUUID(sessionID) {
		return fmt.Errorf("session %q: %w", sessionID, core.ErrNotFound)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1 AND user_id = $2`, sessionID, userID)
	if err != nil {
		return dbErr(err, "revoke session")
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("session %q: %w", sessionID, core.ErrNotFound)
	}
	s.record(ctx, core.AuditEvent{
		Action: AuditSessionRevoke, Target: "user:" + username,
		Details: map[string]any{"session_id": sessionID, "self": userID == core.PrincipalFrom(ctx).ID},
	})
	return nil
}

// RevokeSessions ends every browser session of a user except keep (the
// caller's current session, or "" to end all) and revokes their short-lived
// session tokens: the caller's own, or anyone's with system:users:write.
func (s *Service) RevokeSessions(ctx context.Context, userID, keep string) (int64, error) {
	username, err := s.authorizeUserSessions(ctx, userID, AuditSessionsRevoke, core.ActionWrite)
	if err != nil {
		return 0, err
	}
	var ended int64
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		ended, err = endSessions(ctx, tx, userID, keep)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.record(ctx, core.AuditEvent{
		Action: AuditSessionsRevoke, Target: "user:" + username,
		Details: map[string]any{"sessions_ended": ended, "self": userID == core.PrincipalFrom(ctx).ID, "kept_current": keep != ""},
	})
	return ended, nil
}
