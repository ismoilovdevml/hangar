package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// Policy is a trust policy (schema TrustPolicy).
type Policy struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	// Issuer is the exact iss claim, e.g.
	// https://token.actions.githubusercontent.com.
	Issuer string `json:"issuer"`
	// Audience must be one of the token's aud values.
	Audience   string      `json:"audience"`
	Conditions []Condition `json:"conditions"`
	// ServiceAccount is the account minted tokens act as.
	ServiceAccount string `json:"service_account"`
	// Scopes narrow the minted token (and are all it can ever do, whatever
	// the account holds).
	Scopes []core.Privilege `json:"scopes"`
	// MaxTTLSeconds is the minted token's lifetime.
	MaxTTLSeconds int       `json:"max_ttl_seconds"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	serviceAccountID string
}

// PolicyInput creates or replaces a trust policy (schema TrustPolicyInput).
type PolicyInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Enabled defaults to true.
	Enabled        *bool            `json:"enabled"`
	Issuer         string           `json:"issuer"`
	Audience       string           `json:"audience"`
	Conditions     []Condition      `json:"conditions"`
	ServiceAccount string           `json:"service_account"`
	Scopes         []core.Privilege `json:"scopes"`
	// MaxTTLSeconds defaults to auth.DefaultFederatedTTL; at most
	// auth.MaxFederatedTTL.
	MaxTTLSeconds int `json:"max_ttl_seconds"`
}

// maxScopes bounds a policy's scopes like a token's (auth: 100).
const maxScopes = 100

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validate checks in and returns the fields to store (all but the service
// account id).
func (s *Service) validate(in PolicyInput) (*Policy, error) {
	p := &Policy{
		Name: strings.TrimSpace(in.Name), Description: strings.TrimSpace(in.Description), Enabled: in.Enabled == nil || *in.Enabled,
		Issuer: strings.TrimSpace(in.Issuer), Audience: strings.TrimSpace(in.Audience),
		ServiceAccount: strings.TrimSpace(in.ServiceAccount), MaxTTLSeconds: in.MaxTTLSeconds,
	}
	if p.Audience == "" {
		p.Audience = s.opts.DefaultAudience
	}
	if p.MaxTTLSeconds == 0 {
		p.MaxTTLSeconds = int(auth.DefaultFederatedTTL / time.Second)
	}
	invalid := func(format string, a ...any) error {
		return fmt.Errorf(format+": %w", append(a, core.ErrInvalid)...)
	}
	switch {
	case !nameRe.MatchString(p.Name):
		return nil, invalid("name %q: 1 to 64 letters, digits, '.', '_' or '-', starting with a letter or digit", p.Name)
	case utf8.RuneCountInString(p.Description) > 500 || !utf8.ValidString(p.Description):
		return nil, invalid("description: at most 500 characters")
	case len(p.Issuer) > 512:
		return nil, invalid("issuer: at most 512 bytes")
	case p.Audience == "" || len(p.Audience) > 512 || !utf8.ValidString(p.Audience):
		return nil, invalid("audience: 1 to 512 bytes (the server has no base_url to default to)")
	case p.MaxTTLSeconds < int(auth.MinFederatedTTL/time.Second) || p.MaxTTLSeconds > int(auth.MaxFederatedTTL/time.Second):
		return nil, invalid("max_ttl_seconds %d: between %d and %d", p.MaxTTLSeconds,
			int(auth.MinFederatedTTL/time.Second), int(auth.MaxFederatedTTL/time.Second))
	case len(in.Scopes) == 0:
		return nil, invalid("scopes: at least one is required (for example repo:npm-hosted:write)")
	case len(in.Scopes) > maxScopes:
		return nil, invalid("scopes: at most %d", maxScopes)
	}
	if err := checkURL(p.Issuer, s.opts.AllowHTTPIssuers); err != nil {
		return nil, invalid("issuer %q %s", p.Issuer, err.Error())
	}
	if err := auth.ValidatePrivileges(in.Scopes); err != nil {
		return nil, err
	}
	p.Scopes = slices.Clone(in.Scopes)
	conds, err := ValidateConditions(p.Issuer, in.Conditions)
	if err != nil {
		return nil, invalid("conditions: %s", err.Error())
	}
	p.Conditions = conds
	return p, nil
}

// policyCols selects a Policy from "oidc_trust_policies p JOIN users u".
const policyCols = `p.id::text, p.name, p.description, p.enabled, p.issuer, p.audience, p.conditions,
	u.username, p.service_account_id::text, p.scopes, p.max_ttl_seconds, p.created_at, p.updated_at`

func scanPolicy(row pgx.CollectableRow) (*Policy, error) {
	p := &Policy{}
	var conds []byte
	var scopes []string
	if err := row.Scan(&p.ID, &p.Name, &p.Description, &p.Enabled, &p.Issuer, &p.Audience, &conds,
		&p.ServiceAccount, &p.serviceAccountID, &scopes, &p.MaxTTLSeconds, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(conds, &p.Conditions); err != nil {
		return nil, fmt.Errorf("policy %s: conditions: %w", p.Name, err)
	}
	p.Scopes = make([]core.Privilege, 0, len(scopes))
	for _, str := range scopes {
		pr, err := core.ParsePrivilege(str)
		if err != nil {
			return nil, fmt.Errorf("policy %s: %w", p.Name, err)
		}
		p.Scopes = append(p.Scopes, pr)
	}
	return p, nil
}

func (s *Service) queryPolicies(ctx context.Context, where string, args ...any) ([]*Policy, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+policyCols+`
		FROM oidc_trust_policies p JOIN users u ON u.id = p.service_account_id `+where+` ORDER BY p.name`, args...)
	if err != nil {
		return nil, dbErr(err, "trust policies")
	}
	out, err := pgx.CollectRows(rows, scanPolicy)
	if err != nil {
		return nil, dbErr(err, "trust policies")
	}
	if out == nil {
		out = []*Policy{}
	}
	return out, nil
}

func (s *Service) policy(ctx context.Context, name string) (*Policy, error) {
	ps, err := s.queryPolicies(ctx, `WHERE p.name = $1`, name)
	if err != nil {
		return nil, err
	}
	if len(ps) == 0 {
		return nil, fmt.Errorf("trust policy %q: %w", name, core.ErrNotFound)
	}
	return ps[0], nil
}

// ListPolicies lists the trust policies (system:settings:read).
func (s *Service) ListPolicies(ctx context.Context) ([]*Policy, error) {
	if err := s.authorize(ctx, core.ActionRead, "trust_policy.read", "trust_policies"); err != nil {
		return nil, err
	}
	return s.queryPolicies(ctx, "")
}

// GetPolicy returns one trust policy (system:settings:read).
func (s *Service) GetPolicy(ctx context.Context, name string) (*Policy, error) {
	if err := s.authorize(ctx, core.ActionRead, "trust_policy.read", "trust_policy:"+auditName(name)); err != nil {
		return nil, err
	}
	return s.policy(ctx, name)
}

// CreatePolicy creates a trust policy. It needs system:settings:admin and
// every privilege the policy's scopes grant: a policy hands its scopes to
// whoever holds a matching ID token, so defining one is no wider than
// holding them.
func (s *Service) CreatePolicy(ctx context.Context, in PolicyInput) (*Policy, error) {
	target := "trust_policy:" + auditName(in.Name)
	if err := s.authorize(ctx, core.ActionAdmin, AuditPolicyCreate, target); err != nil {
		return nil, err
	}
	p, err := s.validate(in)
	if err != nil {
		return nil, err
	}
	if err := s.tokens.RequireCoversPrivileges(ctx, AuditPolicyCreate, target, "trust policy "+p.Name, p.Scopes); err != nil {
		return nil, err
	}
	if p.serviceAccountID, err = s.tokens.ServiceAccountID(ctx, p.ServiceAccount); err != nil {
		return nil, err
	}
	conds, err := json.Marshal(p.Conditions)
	if err != nil {
		return nil, err
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO oidc_trust_policies (name, description, enabled, issuer, audience, conditions, service_account_id, scopes, max_ttl_seconds)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id::text`,
		p.Name, p.Description, p.Enabled, p.Issuer, p.Audience, conds, p.serviceAccountID, privilegeStrings(p.Scopes), p.MaxTTLSeconds).Scan(&p.ID)
	if err != nil {
		return nil, dbErr(err, fmt.Sprintf("trust policy %q", p.Name))
	}
	s.record(ctx, core.AuditEvent{Action: AuditPolicyCreate, Target: "trust_policy:" + p.Name, Details: policyDetails(p)})
	return s.policy(ctx, p.Name)
}

// UpdatePolicy replaces a trust policy (renaming it is not possible). The
// caller must cover the scopes it had and the scopes it gets: otherwise a
// narrower administrator could point a powerful policy at their own
// repository. Tokens the policy minted before are revoked.
func (s *Service) UpdatePolicy(ctx context.Context, name string, in PolicyInput) (*Policy, error) {
	target := "trust_policy:" + auditName(name)
	if err := s.authorize(ctx, core.ActionAdmin, AuditPolicyUpdate, target); err != nil {
		return nil, err
	}
	old, err := s.policy(ctx, name)
	if err != nil {
		return nil, err
	}
	if in.Name == "" {
		in.Name = name
	}
	p, err := s.validate(in)
	if err != nil {
		return nil, err
	}
	if p.Name != old.Name {
		return nil, fmt.Errorf("name: a trust policy cannot be renamed: %w", core.ErrInvalid)
	}
	want := append(slices.Clone(old.Scopes), p.Scopes...)
	if err := s.tokens.RequireCoversPrivileges(ctx, AuditPolicyUpdate, target, "trust policy "+p.Name, want); err != nil {
		return nil, err
	}
	if p.serviceAccountID, err = s.tokens.ServiceAccountID(ctx, p.ServiceAccount); err != nil {
		return nil, err
	}
	conds, err := json.Marshal(p.Conditions)
	if err != nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE oidc_trust_policies SET description = $2, enabled = $3, issuer = $4, audience = $5, conditions = $6,
			service_account_id = $7, scopes = $8, max_ttl_seconds = $9, updated_at = now()
		WHERE id = $1`,
		old.ID, p.Description, p.Enabled, p.Issuer, p.Audience, conds, p.serviceAccountID, privilegeStrings(p.Scopes), p.MaxTTLSeconds)
	if err != nil {
		return nil, dbErr(err, fmt.Sprintf("trust policy %q", name))
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("trust policy %q: %w", name, core.ErrNotFound)
	}
	revoked, err := s.tokens.RevokeFederatedTokens(ctx, old.ID)
	if err != nil {
		return nil, err
	}
	details := policyDetails(p)
	details["revoked_tokens"] = revoked
	s.record(ctx, core.AuditEvent{Action: AuditPolicyUpdate, Target: "trust_policy:" + p.Name, Details: details})
	return s.policy(ctx, name)
}

// DeletePolicy deletes a trust policy and the tokens it minted. Like
// changing it, it needs system:settings:admin and the policy's scopes.
func (s *Service) DeletePolicy(ctx context.Context, name string) error {
	target := "trust_policy:" + auditName(name)
	if err := s.authorize(ctx, core.ActionAdmin, AuditPolicyDelete, target); err != nil {
		return err
	}
	old, err := s.policy(ctx, name)
	if err != nil {
		return err
	}
	if err := s.tokens.RequireCoversPrivileges(ctx, AuditPolicyDelete, target, "trust policy "+old.Name, old.Scopes); err != nil {
		return err
	}
	// The tokens it minted go with it (tokens.trust_policy_id cascades).
	tag, err := s.pool.Exec(ctx, `DELETE FROM oidc_trust_policies WHERE id = $1`, old.ID)
	if err != nil {
		return dbErr(err, fmt.Sprintf("trust policy %q", name))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("trust policy %q: %w", name, core.ErrNotFound)
	}
	s.record(ctx, core.AuditEvent{Action: AuditPolicyDelete, Target: "trust_policy:" + old.Name, Details: policyDetails(old)})
	return nil
}

func policyDetails(p *Policy) map[string]any {
	claims := make([]string, 0, len(p.Conditions))
	for _, c := range p.Conditions {
		claims = append(claims, c.Claim)
	}
	return map[string]any{
		"issuer": p.Issuer, "audience": p.Audience, "enabled": p.Enabled, "service_account": p.ServiceAccount,
		"scopes": privilegeStrings(p.Scopes), "max_ttl_seconds": p.MaxTTLSeconds, "condition_claims": claims,
	}
}

func privilegeStrings(ps []core.Privilege) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.String())
	}
	return out
}

// auditName bounds caller-supplied names written to the audit log.
func auditName(s string) string {
	if len(s) > 128 {
		s = s[:128]
	}
	return strings.ToValidUTF8(s, "?")
}

// dbErr maps Postgres errors to core sentinels.
func dbErr(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, core.ErrNotFound)
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return fmt.Errorf("%s: %w", what, core.ErrAlreadyExists)
		case "23514", "22P02", "23502", "23503":
			return fmt.Errorf("%s: %s: %w", what, pg.Message, core.ErrInvalid)
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
