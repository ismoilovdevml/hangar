// Package federation implements the CI OIDC token exchange ("trusted
// publishing"): a CI job (GitHub Actions, GitLab CI id_tokens, any OpenID
// Connect issuer) sends the ID token its platform signed to
// POST /api/v1/auth/oidc/exchange and gets a short-lived Hangar token, so
// pipelines publish without a long-lived secret.
//
// Administrators define trust policies (/api/v1/trust-policies): the issuer
// and audience accepted, conditions on the token's claims (repository, ref,
// environment, project_path ...), the service account the minted token acts
// as, the scopes it is narrowed to and its lifetime. The exchange verifies
// the ID token's signature with the issuer's published keys (discovery and
// JWKS, cached, fetched through the service dial guard), its issuer,
// audience and validity times, allows RS256 and ES256 only, matches it to a
// policy and mints a federated token (auth.Service.IssueFederated). Each ID
// token can be exchanged once (its jti is remembered until it expires).
package federation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// Audit actions recorded by this package.
const (
	AuditExchange     = "auth.oidc.exchange"
	AuditPolicyCreate = "trust_policy.create"
	AuditPolicyUpdate = "trust_policy.update"
	AuditPolicyDelete = "trust_policy.delete"
)

// ExchangePath is the exchange endpoint, relative to the server root.
const ExchangePath = "/api/v1/auth/oidc/exchange"

// Tokens mints federated tokens and checks what an editor may grant
// (implemented by *auth.Service).
type Tokens interface {
	IssueFederated(ctx context.Context, req auth.FederatedTokenRequest) (*auth.FederatedToken, error)
	RevokeFederatedTokens(ctx context.Context, policyID string) (int64, error)
	RequireCoversPrivileges(ctx context.Context, auditAction, target, what string, want []core.Privilege) error
	ServiceAccountID(ctx context.Context, name string) (string, error)
}

// Options are the collaborators of a Service.
type Options struct {
	// Pool, Tokens, AuthZ and Audit are required.
	Pool   *pgxpool.Pool
	Tokens Tokens
	AuthZ  core.AuthZ
	Audit  core.AuditLog
	// DefaultAudience is the audience of policies that name none:
	// server.base_url.
	DefaultAudience string
	// AllowHTTPIssuers accepts plain-http issuers (auth.ci_oidc.
	// allow_http_issuers; development and tests only).
	AllowHTTPIssuers bool
	// DialGuard checks every address issuer requests dial
	// (proxy.Guard.CheckServiceAddr). Nil checks nothing (tests).
	DialGuard func(netip.Addr) error
	// RateLimit bounds exchanges per client address (IPv6 per /64); the
	// server passes auth.login_rate. A non-positive PerMinute is no limit.
	RateLimit auth.RateLimit
	// HTTPClient replaces the guarded client for issuer requests (tests).
	HTTPClient *http.Client
	Logger     *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Service manages trust policies and performs exchanges. Its zero value
// only registers routes (route listings); use New.
type Service struct {
	pool    *pgxpool.Pool
	opts    Options
	tokens  Tokens
	keys    *keyCache
	limiter *auth.RateLimiter
	log     *slog.Logger
	now     func() time.Time
}

// New returns a service over pool (migrated schema).
func New(opts Options) (*Service, error) {
	if opts.Pool == nil || opts.Tokens == nil || opts.AuthZ == nil || opts.Audit == nil {
		return nil, fmt.Errorf("federation: pool, tokens, authz and audit are required: %w", core.ErrInvalid)
	}
	s := &Service{pool: opts.Pool, opts: opts, tokens: opts.Tokens, log: opts.Logger, now: opts.Now}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	client := opts.HTTPClient
	if client == nil {
		client = newHTTPClient(opts.DialGuard, opts.AllowHTTPIssuers)
	}
	s.keys = newKeyCache(client, opts.AllowHTTPIssuers, s.now)
	s.limiter = auth.NewRateLimiter(opts.RateLimit, s.now)
	return s, nil
}

// record appends an audit event; a failing audit log is logged, not
// returned, because the action it describes has already happened.
func (s *Service) record(ctx context.Context, e core.AuditEvent) {
	if err := s.opts.Audit.Record(ctx, e); err != nil {
		s.log.ErrorContext(ctx, "federation: audit record failed", "action", e.Action, "error", err)
	}
}

// authorize checks system:settings:<action> and audits a denial.
func (s *Service) authorize(ctx context.Context, action core.Action, auditAction, target string) error {
	p := core.PrincipalFrom(ctx)
	if err := s.opts.AuthZ.Authorize(ctx, p, action, core.SystemResource(core.SystemSettings)); err != nil {
		if errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrUnauthenticated) {
			s.record(ctx, core.AuditEvent{Action: auditAction, Target: target, Outcome: core.OutcomeDenied,
				Details: map[string]any{"reason": "not_authorized"}})
		}
		return err
	}
	return nil
}
