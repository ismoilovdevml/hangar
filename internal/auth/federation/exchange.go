package federation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// allowedAlgs are the signature algorithms accepted: what GitHub, GitLab
// and common issuers sign with. "none" and the HMAC algorithms are never
// accepted, so a token cannot be "signed" with a public key as an HMAC
// secret.
var allowedAlgs = []jose.SignatureAlgorithm{jose.RS256, jose.ES256}

// clockSkew is the leeway for exp, nbf and iat: go-jose's documented
// default for these checks.
const clockSkew = jwt.DefaultLeeway

// maxIDTokenBytes bounds the ID token accepted. Platform ID tokens are a
// few KiB; this leaves room for large custom claims.
const maxIDTokenBytes = 16 << 10

// Reasons an exchange is refused, in the audit log and the error details.
const (
	ReasonMalformed      = "malformed"
	ReasonUnknownIssuer  = "unknown_issuer"
	ReasonUnknownKey     = "unknown_key"
	ReasonBadSignature   = "bad_signature"
	ReasonExpired        = "expired"
	ReasonNotYetValid    = "not_yet_valid"
	ReasonMissingClaims  = "missing_claims"
	ReasonNoPolicy       = "no_matching_policy"
	ReasonAmbiguous      = "ambiguous_policy"
	ReasonReplayed       = "replayed"
	ReasonAccountRefused = "service_account_refused"
	ReasonRateLimited    = "rate_limited"
)

// ExchangeError is a refused exchange. It wraps core.ErrUnauthenticated
// (core.ErrInvalid for ReasonAmbiguous and ReasonMalformed requests,
// core.ErrForbidden for ReasonAccountRefused).
type ExchangeError struct {
	Reason  string
	Message string
	err     error
}

func (e *ExchangeError) Error() string { return e.Message }

// Unwrap returns the core sentinel of the refusal.
func (e *ExchangeError) Unwrap() error { return e.err }

func refused(reason, msg string) *ExchangeError {
	return &ExchangeError{Reason: reason, Message: msg, err: core.ErrUnauthenticated}
}

// ExchangeInput is the body of POST /auth/oidc/exchange.
type ExchangeInput struct {
	// Token is the CI platform's ID token (a compact JWS).
	Token string `json:"token"`
	// Policy names the trust policy to use; needed only when several match.
	Policy string `json:"policy"`
}

// ExchangeResult is the minted token (schema OIDCExchangeResult).
type ExchangeResult struct {
	Token          string           `json:"token"`
	TokenType      string           `json:"token_type"`
	ExpiresAt      time.Time        `json:"expires_at"`
	ExpiresIn      int              `json:"expires_in"`
	Scopes         []core.Privilege `json:"scopes"`
	ServiceAccount string           `json:"service_account"`
	Policy         string           `json:"policy"`
}

// idToken is a verified ID token.
type idToken struct {
	issuer, subject, jti string
	audience             jwt.Audience
	expires              time.Time
	claims               map[string]any
}

// Exchange verifies a CI platform's ID token and mints a federated token
// for the one trust policy it matches. Every attempt is audited (issuer,
// subject, policy, scopes, outcome; never the token). Refusals are
// *ExchangeError; an issuer whose keys cannot be fetched gives an error
// wrapping core.ErrUpstream; too many attempts from one address a
// core.RetryAfterError.
func (s *Service) Exchange(ctx context.Context, in ExchangeInput) (*ExchangeResult, error) {
	audit := core.AuditEvent{Action: AuditExchange, Target: "trust_policy:", Details: map[string]any{}}
	fail := func(err error) (*ExchangeResult, error) {
		audit.Outcome = core.OutcomeDenied
		var xe *ExchangeError
		if errors.As(err, &xe) {
			audit.Details["reason"] = xe.Reason
		} else {
			audit.Outcome = core.OutcomeError
			audit.Details["error"] = err.Error()
		}
		s.record(ctx, audit)
		return nil, err
	}
	if ok, wait := s.limiter.Allow(auth.AddressKey(core.RequestInfoFrom(ctx).RemoteIP)); !ok {
		audit.Details["reason"] = ReasonRateLimited
		audit.Outcome = core.OutcomeDenied
		s.record(ctx, audit)
		return nil, &core.RetryAfterError{After: wait, Err: fmt.Errorf("too many token exchanges from this address: %w", core.ErrRateLimited)}
	}
	raw := strings.TrimSpace(in.Token)
	if raw == "" || len(raw) > maxIDTokenBytes {
		return fail(&ExchangeError{Reason: ReasonMalformed, err: core.ErrInvalid,
			Message: fmt.Sprintf("token: the CI platform's ID token is required (at most %d bytes)", maxIDTokenBytes)})
	}
	tok, issuer, err := parseIDToken(raw)
	if err != nil {
		return fail(err)
	}
	audit.Target = "issuer:" + auditName(issuer)
	policies, err := s.queryPolicies(ctx, `WHERE p.enabled AND p.issuer = $1`, issuer)
	if err != nil {
		return fail(err)
	}
	if len(policies) == 0 {
		// Unknown issuers are refused before anything is fetched from them.
		audit.Details["issuer"] = auditName(issuer)
		return fail(refused(ReasonUnknownIssuer, "no trust policy accepts tokens of this issuer"))
	}
	audit.Details["issuer"] = issuer
	id, err := s.verify(ctx, tok, issuer)
	if err != nil {
		return fail(err)
	}
	audit.Details["subject"], audit.Details["jti"] = auditName(id.subject), auditName(id.jti)
	p, err := pick(policies, id, strings.TrimSpace(in.Policy))
	if err != nil {
		return fail(err)
	}
	audit.Target = "trust_policy:" + p.Name
	audit.Details["policy"], audit.Details["service_account"] = p.Name, p.ServiceAccount
	audit.Details["scopes"] = privilegeStrings(p.Scopes)
	minted, err := s.tokens.IssueFederated(ctx, auth.FederatedTokenRequest{
		PolicyID: p.ID, PolicyName: p.Name, ServiceAccountID: p.serviceAccountID, Scopes: p.Scopes,
		TTL: time.Duration(p.MaxTTLSeconds) * time.Second, Issuer: id.issuer, Subject: id.subject, JTI: id.jti,
		IDTokenExpires: id.expires.Add(clockSkew),
	})
	switch {
	case errors.Is(err, auth.ErrIDTokenReplayed):
		return fail(refused(ReasonReplayed, "this ID token was already exchanged; request a new one"))
	case errors.Is(err, core.ErrForbidden):
		return fail(&ExchangeError{Reason: ReasonAccountRefused, Message: core.Message(err), err: core.ErrForbidden})
	case err != nil:
		return fail(err)
	}
	audit.Actor = minted.Owner
	audit.Outcome = core.OutcomeSuccess
	audit.Details["token_id"] = minted.ID
	audit.Details["expires_at"] = minted.ExpiresAt
	s.record(ctx, audit)
	return &ExchangeResult{
		Token: minted.Secret, TokenType: "Bearer", ExpiresAt: minted.ExpiresAt,
		ExpiresIn: int(minted.ExpiresAt.Sub(s.now()).Round(time.Second) / time.Second),
		Scopes:    minted.Scopes, ServiceAccount: minted.Owner, Policy: p.Name,
	}, nil
}

// parseIDToken parses a compact JWS signed with an allowed algorithm and
// returns it with its (not yet verified) issuer.
func parseIDToken(raw string) (*jwt.JSONWebToken, string, error) {
	tok, err := jwt.ParseSigned(raw, allowedAlgs)
	if err != nil {
		return nil, "", refused(ReasonMalformed, "the ID token is not a JWS signed with RS256 or ES256")
	}
	if len(tok.Headers) != 1 {
		return nil, "", refused(ReasonMalformed, "the ID token must have exactly one signature")
	}
	var c jwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&c); err != nil || c.Issuer == "" {
		return nil, "", refused(ReasonMalformed, "the ID token has no readable iss claim")
	}
	return tok, c.Issuer, nil
}

// verify checks the signature with the issuer's published key and the
// registered claims: iss, exp (required), nbf and iat (when present) with
// clockSkew, and that sub and jti are present. The audience is checked per
// policy (pick).
func (s *Service) verify(ctx context.Context, tok *jwt.JSONWebToken, issuer string) (*idToken, error) {
	h := tok.Headers[0]
	key, err := s.keys.key(ctx, issuer, h.KeyID, h.Algorithm)
	switch {
	case errors.Is(err, errUnknownKey):
		return nil, refused(ReasonUnknownKey, "the ID token's key id is not among the issuer's published keys")
	case err != nil:
		return nil, err
	}
	if err := tok.Claims(key); err != nil {
		return nil, refused(ReasonBadSignature, "the ID token's signature does not verify")
	}
	// Verified above: the payload is the signed one.
	var std jwt.Claims
	all := map[string]any{}
	if err := tok.UnsafeClaimsWithoutVerification(&std, &all); err != nil {
		return nil, refused(ReasonMalformed, "the ID token's claims are not valid JSON claims")
	}
	now := s.now()
	switch err := std.ValidateWithLeeway(jwt.Expected{Issuer: issuer, Time: now}, clockSkew); {
	case std.Expiry == nil:
		return nil, refused(ReasonMissingClaims, "the ID token has no exp claim")
	case errors.Is(err, jwt.ErrExpired):
		return nil, refused(ReasonExpired, "the ID token has expired; request a new one")
	case errors.Is(err, jwt.ErrNotValidYet) || errors.Is(err, jwt.ErrIssuedInTheFuture):
		return nil, refused(ReasonNotYetValid, "the ID token is not valid yet; check the clocks")
	case err != nil:
		return nil, refused(ReasonMalformed, "the ID token's claims are invalid")
	case std.Subject == "" || std.ID == "":
		return nil, refused(ReasonMissingClaims, "the ID token needs sub and jti claims")
	case len(std.ID) > 256:
		return nil, refused(ReasonMalformed, "the ID token's jti is too long")
	}
	return &idToken{issuer: issuer, subject: std.Subject, jti: std.ID, audience: std.Audience,
		expires: std.Expiry.Time(), claims: all}, nil
}

// pick returns the policy that accepts id: its audience is among the
// token's and all its conditions match. name, when given, selects among
// several. No match and several matches are refused.
func pick(policies []*Policy, id *idToken, name string) (*Policy, error) {
	var matched []*Policy
	for _, p := range policies {
		if name != "" && p.Name != name {
			continue
		}
		if id.audience.Contains(p.Audience) && matchAll(p.Conditions, id.claims) {
			matched = append(matched, p)
		}
	}
	switch len(matched) {
	case 0:
		return nil, refused(ReasonNoPolicy, "no trust policy matches this ID token (check its audience and claims)")
	case 1:
		return matched[0], nil
	}
	return nil, &ExchangeError{Reason: ReasonAmbiguous, err: core.ErrInvalid,
		Message: fmt.Sprintf("%d trust policies match this ID token: name one in \"policy\"", len(matched))}
}
