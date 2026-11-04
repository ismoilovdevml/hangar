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
	if p.PasswordChangeRequired {
		return "", time.Time{}, ErrPasswordChangeRequired
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
	// AllowedCIDRs, when not empty, limits the token to requests from these
	// client networks (addresses or CIDR prefixes).
	AllowedCIDRs []string `json:"allowed_cidrs"`
	// ReadOnly limits the token to reading, whatever its scopes say.
	ReadOnly bool `json:"read_only"`
}

// AuditTokenRotate is recorded when a token is replaced by a new one.
const AuditTokenRotate = "token.rotate"

// tokenPlan is a validated, authorized TokenInput with its owner resolved.
type tokenPlan struct {
	in                            TokenInput
	ownerID, ownerName, ownerKind string
}

// CreateToken creates a token for the caller, or for a service account
// (needs system:tokens:write). The secret is returned once.
func (s *Service) CreateToken(ctx context.Context, in TokenInput) (*TokenInfo, string, error) {
	plan, err := s.planToken(ctx, in)
	if err != nil {
		return nil, "", err
	}
	t, secret, err := s.insertToken(ctx, s.pool, plan)
	if err != nil {
		return nil, "", err
	}
	s.record(ctx, core.AuditEvent{
		Action: AuditTokenCreate, Target: "token:" + t.ID, Details: tokenAuditDetails(t),
	})
	return t, secret, nil
}

// planToken validates in and checks that the caller may create it.
func (s *Service) planToken(ctx context.Context, in TokenInput) (*tokenPlan, error) {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() || !validUUID(p.ID) {
		return nil, fmt.Errorf("create token: %w", core.ErrUnauthenticated)
	}
	// Checked against the account, not the principal: a token or the CLI
	// acting for a user who must change their password may not mint one.
	if err := s.checkPasswordCurrent(ctx, p); err != nil {
		return nil, err
	}
	in.Name = strings.TrimSpace(in.Name)
	now := s.now()
	switch {
	case in.Name == "" || utf8.RuneCountInString(in.Name) > 100:
		return nil, fmt.Errorf("token name must be 1 to 100 characters: %w", core.ErrInvalid)
	case len(in.Scopes) == 0:
		return nil, fmt.Errorf("token needs at least one scope: %w", core.ErrInvalid)
	case len(in.Scopes) > maxPrivileges:
		return nil, fmt.Errorf("token has more than %d scopes: %w", maxPrivileges, core.ErrInvalid)
	case in.ExpiresAt.IsZero():
		return nil, fmt.Errorf("token expires_at is required: %w", core.ErrInvalid)
	case !in.ExpiresAt.After(now):
		return nil, fmt.Errorf("token expires_at must be in the future: %w", core.ErrInvalid)
	case in.ExpiresAt.After(now.Add(s.cfg.MaxTokenLifetime)):
		return nil, fmt.Errorf("token expires_at must be within %s: %w", s.cfg.MaxTokenLifetime, core.ErrInvalid)
	}
	if err := ValidatePrivileges(in.Scopes); err != nil {
		return nil, err
	}
	in.Scopes = compactPrivileges(in.Scopes)
	cidrs, err := normalizeCIDRs(in.AllowedCIDRs)
	if err != nil {
		return nil, err
	}
	in.AllowedCIDRs = cidrs
	if p.Scopes != nil {
		// A scoped token may only mint tokens within its own scopes.
		for _, want := range in.Scopes {
			if !slices.ContainsFunc(p.Scopes, func(have core.Privilege) bool { return covers(have, want) }) {
				return nil, fmt.Errorf("scope %s exceeds the calling token's scopes: %w", want, core.ErrForbidden)
			}
		}
	}
	if p.ReadOnly && !in.ReadOnly {
		return nil, fmt.Errorf("a read-only token may only create read-only tokens: %w", core.ErrForbidden)
	}
	plan := &tokenPlan{in: in, ownerID: p.ID, ownerName: p.Name, ownerKind: KindUser}
	if p.Kind == core.PrincipalServiceAccount {
		plan.ownerKind = KindServiceAccount
	}
	if in.Owner != "" && in.Owner != p.Name {
		if err := s.require(ctx, AuditTokenCreate, core.ActionWrite, core.SystemResource(core.SystemTokens)); err != nil {
			return nil, err
		}
		owner, err := s.userByName(ctx, s.pool, in.Owner)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return nil, fmt.Errorf("owner %q: no such service account: %w", in.Owner, core.ErrInvalid)
			}
			return nil, err
		}
		if owner.Kind != KindServiceAccount {
			return nil, fmt.Errorf("owner %q must be a service account: %w", in.Owner, core.ErrInvalid)
		}
		if owner.Disabled {
			return nil, fmt.Errorf("service account %q is disabled: %w", in.Owner, core.ErrInvalid)
		}
		plan.ownerID, plan.ownerName, plan.ownerKind = owner.ID, owner.Username, owner.Kind
	}
	return plan, nil
}

// insertToken stores a planned token with a new secret.
func (s *Service) insertToken(ctx context.Context, q querier, plan *tokenPlan) (*TokenInfo, string, error) {
	secret, err := newTokenSecret()
	if err != nil {
		return nil, "", fmt.Errorf("create token: %w", err)
	}
	in := plan.in
	t := &TokenInfo{Token: core.Token{
		Name: in.Name, Owner: plan.ownerName, Prefix: secret[:tokenDisplayLen],
		Scopes: slices.Clone(in.Scopes), ExpiresAt: in.ExpiresAt.UTC().Truncate(time.Microsecond),
	}, OwnerKind: plan.ownerKind, AllowedCIDRs: nonNilStrings(in.AllowedCIDRs), ReadOnly: in.ReadOnly}
	err = q.QueryRow(ctx, `
		INSERT INTO tokens (user_id, name, kind, hash, prefix, scopes, expires_at, allowed_cidrs, read_only)
		VALUES ($1, $2, 'personal', $3, $4, $5, $6, $7::cidr[], $8) RETURNING id::text, created_at`,
		plan.ownerID, t.Name, hashSecret(secret), t.Prefix, privilegeStrings(t.Scopes), t.ExpiresAt,
		in.AllowedCIDRs, in.ReadOnly).Scan(&t.ID, &t.CreatedAt)
	if err != nil {
		return nil, "", dbErr(err, "create token")
	}
	return t, secret, nil
}

func tokenAuditDetails(t *TokenInfo) map[string]any {
	d := map[string]any{
		"name": t.Name, "owner": t.Owner, "owner_kind": t.OwnerKind,
		"scopes": privilegeStrings(t.Scopes), "expires_at": t.ExpiresAt,
	}
	if len(t.AllowedCIDRs) > 0 {
		d["allowed_cidrs"] = t.AllowedCIDRs
	}
	if t.ReadOnly {
		d["read_only"] = true
	}
	return d
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// RotateInput replaces a token by a new one.
type RotateInput struct {
	// RevokeOldAfter, when set, ends the old token: at once for 0, or that
	// long from now (never later than it would have expired anyway). When
	// nil the old token keeps working until it is revoked or expires.
	RevokeOldAfter *time.Duration
}

// RotateResult is a rotated token: the replacement with its secret, and what
// became of the old one.
type RotateResult struct {
	Token  *TokenInfo
	Secret string
	// Old is the replaced token as it is now: ExpiresAt moved earlier when
	// it ends later, RevokedNow when it was revoked at once.
	Old        *TokenInfo
	RevokedNow bool
}

// RotateToken creates a replacement for a token with the same name, owner,
// scopes, restrictions and lifetime (capped at the server maximum), and
// optionally ends the old one. The caller needs what CreateToken needs for
// the owner (another principal's token: system:tokens:write, and only
// service accounts), and system:tokens:delete to end someone else's token.
func (s *Service) RotateToken(ctx context.Context, id string, in RotateInput) (*RotateResult, error) {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() {
		return nil, fmt.Errorf("rotate token: %w", core.ErrUnauthenticated)
	}
	notFound := fmt.Errorf("token %q: %w", id, core.ErrNotFound)
	if !validUUID(id) {
		return nil, notFound
	}
	if in.RevokeOldAfter != nil && *in.RevokeOldAfter < 0 {
		return nil, fmt.Errorf("revoke_old_after must not be negative: %w", core.ErrInvalid)
	}
	old, err := s.tokenInfo(ctx, s.pool, id)
	if err != nil {
		return nil, err
	}
	if old.Owner != p.Name {
		// Hide other people's tokens from callers who may not manage them.
		if s.authz.Authorize(ctx, p, core.ActionWrite, core.SystemResource(core.SystemTokens)) != nil {
			s.record(ctx, core.AuditEvent{Action: AuditTokenRotate, Target: "token:" + id, Outcome: core.OutcomeDenied})
			return nil, notFound
		}
		if old.OwnerKind != KindServiceAccount {
			return nil, fmt.Errorf("only %s can rotate their own token: %w", old.Owner, core.ErrForbidden)
		}
		if in.RevokeOldAfter != nil {
			if err := s.require(ctx, AuditTokenRotate, core.ActionDelete, core.SystemResource(core.SystemTokens)); err != nil {
				return nil, err
			}
		}
	}
	now := s.now()
	lifetime := min(max(old.ExpiresAt.Sub(old.CreatedAt), time.Hour), s.cfg.MaxTokenLifetime)
	plan, err := s.planToken(ctx, TokenInput{
		Name: old.Name, Owner: old.Owner, Scopes: old.Scopes, ExpiresAt: now.Add(lifetime),
		AllowedCIDRs: old.AllowedCIDRs, ReadOnly: old.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	res := &RotateResult{}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if res.Token, res.Secret, err = s.insertToken(ctx, tx, plan); err != nil {
			return err
		}
		if in.RevokeOldAfter != nil {
			at := now.Add(*in.RevokeOldAfter).UTC().Truncate(time.Microsecond)
			res.RevokedNow = *in.RevokeOldAfter == 0
			if res.RevokedNow {
				_, err = tx.Exec(ctx, `UPDATE tokens SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, now)
			} else {
				_, err = tx.Exec(ctx, `UPDATE tokens SET expires_at = least(expires_at, $2) WHERE id = $1 AND revoked_at IS NULL`, id, at)
			}
			if err != nil {
				return dbErr(err, "end old token")
			}
		}
		res.Old, err = s.tokenInfo(ctx, tx, id)
		if errors.Is(err, core.ErrNotFound) && res.RevokedNow {
			old.ExpiresAt, err = now.UTC().Truncate(time.Microsecond), nil
			res.Old = old
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	details := tokenAuditDetails(res.Token)
	details["replaces"] = id
	if in.RevokeOldAfter != nil {
		details["old_ends_at"] = res.Old.ExpiresAt
	}
	s.record(ctx, core.AuditEvent{Action: AuditTokenRotate, Target: "token:" + res.Token.ID, Details: details})
	if res.RevokedNow {
		s.record(ctx, core.AuditEvent{
			Action: AuditTokenRevoke, Target: "token:" + id,
			Details: map[string]any{"name": old.Name, "owner": old.Owner, "reason": "rotated"},
		})
	}
	return res, nil
}

// tokenInfo loads one live (not revoked) personal or service-account token.
func (s *Service) tokenInfo(ctx context.Context, q querier, id string) (*TokenInfo, error) {
	rows, err := q.Query(ctx, `
		SELECT `+tokenInfoCols+`
		FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.id = $1 AND t.kind = 'personal' AND t.revoked_at IS NULL`, id)
	if err != nil {
		return nil, dbErr(err, "token")
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanTokenInfo)
	if err != nil {
		return nil, dbErr(err, fmt.Sprintf("token %q", id))
	}
	return t, nil
}

// TokenInfo is a personal access or service-account token as listed: the
// public core.Token plus where it was last used and its owner's kind.
type TokenInfo struct {
	core.Token
	// OwnerKind is "user" or "service_account".
	OwnerKind string `json:"owner_kind"`
	// LastUsedIP is the client address of the latest use, if known.
	LastUsedIP *string `json:"last_used_ip"`
	// AllowedCIDRs are the client networks the token works from; empty
	// means any.
	AllowedCIDRs []string `json:"allowed_cidrs"`
	// ReadOnly tokens can only read, whatever their scopes say.
	ReadOnly bool `json:"read_only"`
}

// Token states for TokenQuery.Status.
const (
	TokenActive  = "active"  // not expired
	TokenExpired = "expired" // past expires_at
)

// TokenQuery filters ListTokens.
type TokenQuery struct {
	// Owner selects another principal's tokens (needs system:tokens:read);
	// empty means the caller's.
	Owner string
	// All lists every principal's tokens (needs system:tokens:read); Owner
	// then narrows the list to one owner.
	All bool
	// OwnerKind narrows an All listing to "user" or "service_account" owners.
	OwnerKind string
	// Status is TokenActive, TokenExpired or empty for both.
	Status string
	// Q is a substring of the token name.
	Q      string
	Limit  int
	Cursor string
}

func (q TokenQuery) validate() error {
	switch q.Status {
	case "", TokenActive, TokenExpired:
	default:
		return fmt.Errorf("status %q: want active or expired: %w", q.Status, core.ErrInvalid)
	}
	if q.OwnerKind != "" && q.OwnerKind != KindUser && q.OwnerKind != KindServiceAccount {
		return fmt.Errorf("owner_kind %q: want user or service_account: %w", q.OwnerKind, core.ErrInvalid)
	}
	if len(q.Q) > 100 || len(q.Owner) > 256 {
		return fmt.Errorf("filter is too long: %w", core.ErrInvalid)
	}
	return nil
}

// tokenInfoCols selects a TokenInfo from "tokens t JOIN users u"; scan with scanTokenInfo.
const tokenInfoCols = //nolint:gosec // G101: a column list, not a credential
`t.id::text, t.name, u.username, u.kind, t.prefix, coalesce(t.scopes, '{}'), t.expires_at,
	t.created_at, t.last_used_at, host(t.last_used_ip), coalesce(t.allowed_cidrs::text[], '{}'), t.read_only`

func scanTokenInfo(r pgx.CollectableRow) (*TokenInfo, error) {
	t := &TokenInfo{}
	var scopes []string
	if err := r.Scan(&t.ID, &t.Name, &t.Owner, &t.OwnerKind, &t.Prefix, &scopes, &t.ExpiresAt,
		&t.CreatedAt, &t.LastUsedAt, &t.LastUsedIP, &t.AllowedCIDRs, &t.ReadOnly); err != nil {
		return nil, err
	}
	t.Scopes = parsePrivileges(scopes)
	return t, nil
}

// ListTokens lists personal and service-account tokens that are not revoked
// (expired ones included), newest first. Secrets are never returned.
func (s *Service) ListTokens(ctx context.Context, q TokenQuery) (core.Page[*TokenInfo], error) {
	var page core.Page[*TokenInfo]
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() {
		return page, fmt.Errorf("list tokens: %w", core.ErrUnauthenticated)
	}
	if err := q.validate(); err != nil {
		return page, err
	}
	owner := p.Name
	if q.All || (q.Owner != "" && q.Owner != p.Name) {
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
	var pattern *string
	if q.Q != "" {
		v := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q.Q) + "%"
		pattern = &v
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+tokenInfoCols+`
		FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.kind = 'personal' AND t.revoked_at IS NULL
		  AND ($1 = '' OR u.username = $1)
		  AND ($2::timestamptz IS NULL OR (t.created_at, t.id) < ($2, $3::uuid))
		  AND ($5 = '' OR u.kind = $5)
		  AND ($6 = '' OR ($6 = 'active' AND t.expires_at > $8) OR ($6 = 'expired' AND t.expires_at <= $8))
		  AND ($7::text IS NULL OR t.name ILIKE $7)
		ORDER BY t.created_at DESC, t.id DESC
		LIMIT $4`, owner, curAt, curID, limit+1, q.OwnerKind, q.Status, pattern, s.now())
	if err != nil {
		return page, dbErr(err, "list tokens")
	}
	page.Items, err = pgx.CollectRows(rows, scanTokenInfo)
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

// maxTokenWarnings bounds the warnings returned with the session.
const maxTokenWarnings = 20

// TokenWarnings returns the live tokens that expire within
// Config.TokenExpiryWarning, soonest first: the caller's own and, for
// holders of system:tokens:read, those of service accounts (which nobody
// else would notice expiring).
func (s *Service) TokenWarnings(ctx context.Context) ([]*TokenInfo, error) {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() || !validUUID(p.ID) {
		return []*TokenInfo{}, nil
	}
	serviceAccounts := s.authz.Authorize(ctx, p, core.ActionRead, core.SystemResource(core.SystemTokens)) == nil
	now := s.now()
	rows, err := s.pool.Query(ctx, `
		SELECT `+tokenInfoCols+`
		FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE t.kind = 'personal' AND t.revoked_at IS NULL AND NOT u.disabled
		  AND t.expires_at > $2 AND t.expires_at <= $3
		  AND (t.user_id = $1 OR ($4 AND u.kind = 'service_account'))
		ORDER BY t.expires_at, t.id
		LIMIT $5`, p.ID, now, now.Add(s.cfg.TokenExpiryWarning), serviceAccounts, maxTokenWarnings)
	if err != nil {
		return nil, dbErr(err, "token warnings")
	}
	out, err := pgx.CollectRows(rows, scanTokenInfo)
	return nonNil(out), dbErr(err, "token warnings")
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

// CurrentToken describes the token a request authenticated with, for
// GET /auth/session: what the token is and how far it reaches.
type CurrentToken struct {
	ID string `json:"id"`
	// Kind is "personal" (personal access and service-account tokens) or
	// "session" (short-lived tokens minted for registry logins).
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	// Scopes are the token's effective scopes (narrowed to read for
	// read-only tokens); nil means the owner's full rights.
	Scopes       []core.Privilege `json:"scopes"`
	ExpiresAt    time.Time        `json:"expires_at"`
	ReadOnly     bool             `json:"read_only"`
	AllowedCIDRs []string         `json:"allowed_cidrs"`
}

// CurrentToken returns the token the context principal authenticated with,
// or nil when it did not use one.
func (s *Service) CurrentToken(ctx context.Context) (*CurrentToken, error) {
	p := core.PrincipalFrom(ctx)
	if p.IsAnonymous() || p.TokenID == "" {
		return nil, nil
	}
	t := &CurrentToken{ID: p.TokenID, Scopes: p.Scopes, ReadOnly: p.ReadOnly}
	err := s.pool.QueryRow(ctx, `
		SELECT t.kind, t.name, t.prefix, least(t.expires_at, coalesce(pt.expires_at, t.expires_at)),
			coalesce(coalesce(t.allowed_cidrs, pt.allowed_cidrs)::text[], '{}')
		FROM tokens t LEFT JOIN tokens pt ON pt.id = t.parent_id
		WHERE t.id = $1`, p.TokenID).Scan(&t.Kind, &t.Name, &t.Prefix, &t.ExpiresAt, &t.AllowedCIDRs)
	if err != nil {
		return nil, dbErr(err, "current token")
	}
	return t, nil
}
