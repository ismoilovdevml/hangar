package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Federated tokens are minted by the CI OIDC token exchange (package
// federation): a CI job proves who it is with an ID token its platform
// signed, and gets a short-lived token of the service account a trust
// policy names, narrowed to the policy's scopes. They are kind "federated":
// never listed with personal tokens, never rotated, and a request
// authenticated with one cannot create or rotate other tokens. Session
// tokens minted from one (registry logins) cannot outlive it.

// TokenKindFederated is the tokens.kind of tokens minted by an exchange.
const TokenKindFederated = "federated"

// Lifetimes of federated tokens: a policy's max TTL defaults to
// DefaultFederatedTTL and can be at most MaxFederatedTTL.
const (
	DefaultFederatedTTL = 15 * time.Minute
	MaxFederatedTTL     = time.Hour
	// MinFederatedTTL keeps a policy's max TTL long enough to use the token
	// (the column check of migration 00350 agrees).
	MinFederatedTTL = time.Minute
)

// ErrIDTokenReplayed is returned when an ID token (issuer and jti) was
// already exchanged. It wraps core.ErrUnauthenticated.
var ErrIDTokenReplayed = fmt.Errorf("the ID token was already exchanged: %w", core.ErrUnauthenticated)

// errFederatedCaller refuses token management through a federated token.
var errFederatedCaller = fmt.Errorf("a token minted by a CI OIDC exchange cannot create or rotate tokens: %w", core.ErrForbidden)

// FederatedTokenRequest describes the token an exchange mints.
type FederatedTokenRequest struct {
	// PolicyID and PolicyName identify the trust policy that matched.
	PolicyID, PolicyName string
	// ServiceAccountID is the account the token acts as; it must be an
	// enabled service account.
	ServiceAccountID string
	// Scopes narrow the account's rights; at least one.
	Scopes []core.Privilege
	// TTL is the token's lifetime, at most MaxFederatedTTL.
	TTL time.Duration
	// Issuer, Subject and JTI come from the verified ID token. The pair
	// (Issuer, JTI) is remembered until IDTokenExpires, so the ID token
	// cannot be exchanged twice.
	Issuer, Subject, JTI string
	IDTokenExpires       time.Time
}

// FederatedToken is a minted token. Secret is shown once.
type FederatedToken struct {
	ID        string
	Secret    string
	Owner     string
	Scopes    []core.Privilege
	ExpiresAt time.Time
}

// IssueFederated mints a federated token for a trust policy that matched a
// verified ID token. The ID token's jti is recorded in the same
// transaction, so a replay fails with ErrIDTokenReplayed and a failed mint
// does not use the ID token up. A service account that is missing, disabled
// or not a service account fails with core.ErrForbidden. The caller audits
// the exchange.
func (s *Service) IssueFederated(ctx context.Context, req FederatedTokenRequest) (*FederatedToken, error) {
	switch {
	case !validUUID(req.PolicyID) || !validUUID(req.ServiceAccountID):
		return nil, fmt.Errorf("issue federated token: policy and service account ids are required: %w", core.ErrInvalid)
	case len(req.Scopes) == 0:
		return nil, fmt.Errorf("issue federated token: at least one scope is required: %w", core.ErrInvalid)
	case req.TTL <= 0 || req.TTL > MaxFederatedTTL:
		return nil, fmt.Errorf("issue federated token: ttl %s outside (0, %s]: %w", req.TTL, MaxFederatedTTL, core.ErrInvalid)
	case req.Issuer == "" || req.Subject == "" || req.JTI == "":
		return nil, fmt.Errorf("issue federated token: issuer, subject and jti are required: %w", core.ErrInvalid)
	}
	if err := ValidatePrivileges(req.Scopes); err != nil {
		return nil, err
	}
	scopes := compactPrivileges(req.Scopes)
	now := s.now()
	expires := now.Add(req.TTL).UTC().Truncate(time.Microsecond)
	secret, err := newTokenSecret()
	if err != nil {
		return nil, fmt.Errorf("issue federated token: %w", err)
	}
	claims, err := json.Marshal(map[string]any{"federated": map[string]any{
		"issuer": req.Issuer, "subject": truncate(req.Subject, 512), "policy": req.PolicyName,
	}})
	if err != nil {
		return nil, fmt.Errorf("issue federated token: claims: %w", err)
	}
	out := &FederatedToken{Secret: secret, Scopes: scopes, ExpiresAt: expires}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO oidc_exchanged_ids (issuer, jti, expires_at) VALUES ($1, $2, $3)
			ON CONFLICT (issuer, jti) DO NOTHING`, req.Issuer, req.JTI, req.IDTokenExpires.UTC())
		if err != nil {
			return dbErr(err, "record ID token")
		}
		if tag.RowsAffected() == 0 {
			return ErrIDTokenReplayed
		}
		u, err := s.userByID(ctx, tx, req.ServiceAccountID)
		if errors.Is(err, core.ErrNotFound) {
			return fmt.Errorf("the trust policy's service account no longer exists: %w", core.ErrForbidden)
		}
		if err != nil {
			return err
		}
		if u.Kind != KindServiceAccount || u.Disabled {
			return fmt.Errorf("the trust policy's service account %q is disabled or not a service account: %w", u.Username, core.ErrForbidden)
		}
		out.Owner = u.Username
		return tx.QueryRow(ctx, `
			INSERT INTO tokens (user_id, name, kind, hash, prefix, scopes, claims, expires_at, trust_policy_id)
			VALUES ($1, $2, 'federated', $3, $4, $5, $6, $7, $8) RETURNING id::text`,
			u.ID, truncate("oidc:"+req.PolicyName, 200), hashSecret(secret), secret[:tokenDisplayLen],
			privilegeStrings(scopes), claims, expires, req.PolicyID).Scan(&out.ID)
	})
	if err != nil {
		if errors.Is(err, ErrIDTokenReplayed) || errors.Is(err, core.ErrForbidden) {
			return nil, err
		}
		return nil, dbErr(err, "issue federated token")
	}
	return out, nil
}

// RevokeFederatedTokens revokes the live tokens a trust policy minted, when
// the policy changes or is disabled; session tokens minted from them stop
// working with them. It returns how many were revoked.
func (s *Service) RevokeFederatedTokens(ctx context.Context, policyID string) (int64, error) {
	if !validUUID(policyID) {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE tokens SET revoked_at = $2
		WHERE trust_policy_id = $1 AND kind = 'federated' AND revoked_at IS NULL AND expires_at > $2`, policyID, s.now())
	if err != nil {
		return 0, dbErr(err, "revoke federated tokens")
	}
	return tag.RowsAffected(), nil
}

// RequireCoversPrivileges fails with core.ErrForbidden, and audits the
// denial as auditAction on target, unless the context principal covers
// every privilege of want (see requireCovers). Packages that hand out
// privileges indirectly (trust policies granting token scopes) use it, so
// configuring them is no wider than holding the privileges.
func (s *Service) RequireCoversPrivileges(ctx context.Context, auditAction, target, what string, want []core.Privilege) error {
	return s.requireCovers(ctx, auditAction, target, what, want)
}

// ServiceAccountID returns the id of the enabled service account name, or
// core.ErrInvalid naming the problem. It does not authorize: callers that
// let a principal pick an account check coverage of what it is used for.
func (s *Service) ServiceAccountID(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 256 {
		return "", fmt.Errorf("service_account is required: %w", core.ErrInvalid)
	}
	u, err := s.userByName(ctx, s.pool, name)
	if errors.Is(err, core.ErrNotFound) {
		return "", fmt.Errorf("service_account %q: no such service account: %w", name, core.ErrInvalid)
	}
	if err != nil {
		return "", err
	}
	if u.Kind != KindServiceAccount {
		return "", fmt.Errorf("service_account %q is not a service account: %w", name, core.ErrInvalid)
	}
	if u.Disabled {
		return "", fmt.Errorf("service account %q is disabled: %w", name, core.ErrInvalid)
	}
	return u.ID, nil
}
