package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Issue implements core.TokenIssuer: it mints a short-lived session token for
// p. The token's rights are p's roles narrowed by req.Scopes and, when p
// itself authenticated with a token, by that token's scopes; the new token
// also stops working when that token is revoked or expires.
func (s *Service) Issue(ctx context.Context, p *core.Principal, req core.TokenRequest) (string, time.Time, error) {
	if p.IsAnonymous() || !validUUID(p.ID) {
		return "", time.Time{}, fmt.Errorf("issue token: %w", core.ErrUnauthenticated)
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = s.cfg.SessionTokenTTL
	}
	ttl = min(ttl, s.cfg.MaxSessionTokenTTL)
	now := s.now()
	expires := now.Add(ttl).UTC().Truncate(time.Microsecond)

	var scopes []core.Privilege
	switch {
	case p.Scopes == nil:
		scopes = req.Scopes
	case req.Scopes == nil:
		scopes = p.Scopes
	default:
		scopes = IntersectScopes(p.Scopes, req.Scopes)
	}
	var scopeCol []string // nil stores NULL: the owner's full rights
	if scopes != nil {
		scopeCol = privilegeStrings(scopes)
	}

	var parentID *string
	if p.TokenID != "" {
		var root string
		var parentExp time.Time
		err := s.pool.QueryRow(ctx, `SELECT coalesce(parent_id, id)::text, expires_at FROM tokens WHERE id = $1 AND revoked_at IS NULL`,
			p.TokenID).Scan(&root, &parentExp)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("issue token: parent: %w", dbErr(err, "token"))
		}
		parentID = &root
		if parentExp.Before(expires) {
			expires = parentExp
		}
	}
	claims := []byte("{}")
	if len(req.Extra) > 0 {
		b, err := json.Marshal(req.Extra)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("issue token: claims: %w: %w", err, core.ErrInvalid)
		}
		claims = b
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "session"
	}
	secret, err := newTokenSecret()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("issue token: %w", err)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO tokens (user_id, name, kind, hash, prefix, scopes, claims, expires_at, parent_id)
		VALUES ($1, $2, 'session', $3, $4, $5, $6, $7, $8) RETURNING id::text`,
		p.ID, truncate(name, 200), hashSecret(secret), secret[:tokenDisplayLen], scopeCol, claims, expires, parentID).Scan(&id)
	if err != nil {
		return "", time.Time{}, dbErr(err, "issue token")
	}
	s.record(core.WithPrincipal(ctx, p), core.AuditEvent{
		Action: AuditTokenIssue, Target: "token:" + id,
		Details: map[string]any{"name": name, "kind": "session", "expires_at": expires, "scopes": scopeCol},
	})
	return secret, expires, nil
}

// TokenInput creates a personal access or service-account token.
type TokenInput struct {
	Name string `json:"name"`
	// Owner is a service account name; empty (or the caller's name) means the caller.
	Owner     string           `json:"owner"`
	Scopes    []core.Privilege `json:"scopes"`
	ExpiresAt time.Time        `json:"expires_at"`
}

// CreateToken creates a token for the caller, or for a service account
// (needs system:tokens:write). The secret is returned once.
func (s *Service) CreateToken(ctx context.Context, in TokenInput) (*core.Token, string, error) {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() || !validUUID(p.ID) {
		return nil, "", fmt.Errorf("create token: %w", core.ErrUnauthenticated)
	}
	in.Name = strings.TrimSpace(in.Name)
	now := s.now()
	switch {
	case in.Name == "" || utf8.RuneCountInString(in.Name) > 100:
		return nil, "", fmt.Errorf("token name must be 1 to 100 characters: %w", core.ErrInvalid)
	case len(in.Scopes) == 0:
		return nil, "", fmt.Errorf("token needs at least one scope: %w", core.ErrInvalid)
	case in.ExpiresAt.IsZero():
		return nil, "", fmt.Errorf("token expires_at is required: %w", core.ErrInvalid)
	case !in.ExpiresAt.After(now):
		return nil, "", fmt.Errorf("token expires_at must be in the future: %w", core.ErrInvalid)
	case in.ExpiresAt.After(now.Add(s.cfg.MaxTokenLifetime)):
		return nil, "", fmt.Errorf("token expires_at must be within %s: %w", s.cfg.MaxTokenLifetime, core.ErrInvalid)
	}
	if p.Scopes != nil {
		// A scoped token may only mint tokens within its own scopes.
		for _, want := range in.Scopes {
			if !slices.ContainsFunc(p.Scopes, func(have core.Privilege) bool { return covers(have, want) }) {
				return nil, "", fmt.Errorf("scope %s exceeds the calling token's scopes: %w", want, core.ErrForbidden)
			}
		}
	}
	ownerID, ownerName := p.ID, p.Name
	if in.Owner != "" && in.Owner != p.Name {
		if err := s.require(ctx, AuditTokenCreate, core.ActionWrite, core.SystemResource(core.SystemTokens)); err != nil {
			return nil, "", err
		}
		owner, err := s.userByName(ctx, s.pool, in.Owner)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return nil, "", fmt.Errorf("owner %q: no such service account: %w", in.Owner, core.ErrInvalid)
			}
			return nil, "", err
		}
		if owner.Kind != KindServiceAccount {
			return nil, "", fmt.Errorf("owner %q must be a service account: %w", in.Owner, core.ErrInvalid)
		}
		if owner.Disabled {
			return nil, "", fmt.Errorf("service account %q is disabled: %w", in.Owner, core.ErrInvalid)
		}
		ownerID, ownerName = owner.ID, owner.Username
	}
	secret, err := newTokenSecret()
	if err != nil {
		return nil, "", fmt.Errorf("create token: %w", err)
	}
	t := &core.Token{
		Name: in.Name, Owner: ownerName, Prefix: secret[:tokenDisplayLen],
		Scopes: slices.Clone(in.Scopes), ExpiresAt: in.ExpiresAt.UTC().Truncate(time.Microsecond),
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO tokens (user_id, name, kind, hash, prefix, scopes, expires_at)
		VALUES ($1, $2, 'personal', $3, $4, $5, $6) RETURNING id::text, created_at`,
		ownerID, t.Name, hashSecret(secret), t.Prefix, privilegeStrings(t.Scopes), t.ExpiresAt).Scan(&t.ID, &t.CreatedAt)
	if err != nil {
		return nil, "", dbErr(err, "create token")
	}
	s.record(ctx, core.AuditEvent{
		Action: AuditTokenCreate, Target: "token:" + t.ID,
		Details: map[string]any{"name": t.Name, "owner": ownerName, "scopes": privilegeStrings(t.Scopes), "expires_at": t.ExpiresAt},
	})
	return t, secret, nil
}

// TokenQuery filters ListTokens.
type TokenQuery struct {
	// Owner selects another principal's tokens (needs system:tokens:read);
	// empty means the caller's.
	Owner  string
	Limit  int
	Cursor string
}

// ListTokens lists personal and service-account tokens that are not revoked
// (expired ones included), newest first. Secrets are never returned.
func (s *Service) ListTokens(ctx context.Context, q TokenQuery) (core.Page[*core.Token], error) {
	var page core.Page[*core.Token]
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() {
		return page, fmt.Errorf("list tokens: %w", core.ErrUnauthenticated)
	}
	owner := p.Name
	if q.Owner != "" && q.Owner != p.Name {
		if err := s.require(ctx, "token.list", core.ActionRead, core.SystemResource(core.SystemTokens)); err != nil {
			return page, err
		}
		owner = q.Owner
	}
	limit := pageLimit(q.Limit)
	curAt, curID, err := decodeTimeCursor(q.Cursor)
	if err != nil {
		return page, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text, t.name, u.username, t.prefix, coalesce(t.scopes, '{}'), t.expires_at, t.created_at, t.last_used_at
		FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE u.username = $1 AND t.kind = 'personal' AND t.revoked_at IS NULL
		  AND ($2::timestamptz IS NULL OR (t.created_at, t.id) < ($2, $3::uuid))
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT $4`, owner, curAt, curID, limit+1)
	if err != nil {
		return page, dbErr(err, "list tokens")
	}
	page.Items, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (*core.Token, error) {
		t := &core.Token{}
		var scopes []string
		if err := r.Scan(&t.ID, &t.Name, &t.Owner, &t.Prefix, &scopes, &t.ExpiresAt, &t.CreatedAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		t.Scopes = parsePrivileges(scopes)
		return t, nil
	})
	if err != nil {
		return page, dbErr(err, "list tokens")
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeTimeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// RevokeToken revokes one of the caller's tokens, or anyone's with
// system:tokens:delete. Tokens minted from it stop working too.
func (s *Service) RevokeToken(ctx context.Context, id string) error {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() {
		return fmt.Errorf("revoke token: %w", core.ErrUnauthenticated)
	}
	if !validUUID(id) {
		return fmt.Errorf("token %q: %w", id, core.ErrNotFound)
	}
	var ownerID, ownerName, name string
	err := s.pool.QueryRow(ctx, `
		SELECT t.user_id::text, u.username, t.name FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.id = $1 AND t.kind = 'personal' AND t.revoked_at IS NULL`, id).Scan(&ownerID, &ownerName, &name)
	if err != nil {
		return dbErr(err, fmt.Sprintf("token %q", id))
	}
	if ownerID != p.ID {
		if err := s.require(ctx, AuditTokenRevoke, core.ActionDelete, core.SystemResource(core.SystemTokens)); err != nil {
			if errors.Is(err, core.ErrForbidden) {
				// Do not reveal that someone else's token id exists.
				return fmt.Errorf("token %q: %w", id, core.ErrNotFound)
			}
			return err
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tokens SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, s.now()); err != nil {
		return dbErr(err, "revoke token")
	}
	s.record(ctx, core.AuditEvent{
		Action: AuditTokenRevoke, Target: "token:" + id, Details: map[string]any{"name": name, "owner": ownerName},
	})
	return nil
}

func pageLimit(n int) int {
	switch {
	case n <= 0:
		return core.DefaultPageSize
	case n > core.MaxPageSize:
		return core.MaxPageSize
	}
	return n
}

// Cursors are opaque base64url strings; time cursors are "<unix-nanos>|<uuid>".

func encodeTimeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%d|%s", t.UnixNano(), id))
}

func decodeTimeCursor(c string) (*time.Time, *string, error) {
	if c == "" {
		return nil, nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err == nil {
		var nanos int64
		var id string
		ts, rest, ok := strings.Cut(string(b), "|")
		if ok {
			if _, err = fmt.Sscan(ts, &nanos); err == nil && validUUID(rest) {
				id = rest
				t := time.Unix(0, nanos).UTC()
				return &t, &id, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("invalid cursor: %w", core.ErrInvalid)
}

func encodeStringCursor(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func decodeStringCursor(c string) (*string, error) {
	if c == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || !utf8.Valid(b) {
		return nil, fmt.Errorf("invalid cursor: %w", core.ErrInvalid)
	}
	s := string(b)
	return &s, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
