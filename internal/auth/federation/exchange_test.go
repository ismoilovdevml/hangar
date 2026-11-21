package federation_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
	"github.com/ismoilovdevml/hangar/internal/auth/federation"
	"github.com/ismoilovdevml/hangar/internal/auth/federation/federationtest"
	"github.com/ismoilovdevml/hangar/internal/core"
)

const audience = "https://hangar.example.test"

var npmWrite = core.MustParsePrivilege("repo:npm-hosted:write")

// env is an auth service over a fresh schema, a service account "ci-bot"
// (role Publisher), a TLS issuer and the exchange service trusting it.
type env struct {
	*authtest.Env
	admin context.Context
	is    *federationtest.Issuer
	svc   *federation.Service
}

func newEnv(t *testing.T, opts ...func(*federation.Options)) *env {
	t.Helper()
	ae := authtest.New(t, auth.Config{})
	e := &env{Env: ae, admin: ae.Admin(t), is: federationtest.New(t, false)}
	if _, err := ae.Svc.CreateUser(e.admin, auth.UserInput{Username: "ci-bot", Kind: auth.KindServiceAccount,
		Roles: []string{"Publisher"}}); err != nil {
		t.Fatal(err)
	}
	o := federation.Options{Pool: ae.Pool, Tokens: ae.Svc, AuthZ: ae.Svc.Authorizer(), Audit: ae.Audit,
		DefaultAudience: audience, HTTPClient: e.is.Client(), Now: ae.Clock.Now}
	for _, f := range opts {
		f(&o)
	}
	svc, err := federation.New(o)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

// policy creates a policy for the issuer, repository acme/app on any
// branch, granting npmWrite; mod adjusts the input first.
func (e *env) policy(t *testing.T, name string, mod func(*federation.PolicyInput)) *federation.Policy {
	t.Helper()
	in := federation.PolicyInput{
		Name: name, Issuer: e.is.URL, ServiceAccount: "ci-bot", Scopes: []core.Privilege{npmWrite},
		Conditions: []federation.Condition{
			{Claim: "repository", Values: []string{"acme/app"}},
			{Claim: "ref", Match: federation.MatchGlob, Values: []string{"refs/heads/*"}},
		},
	}
	if mod != nil {
		mod(&in)
	}
	p, err := e.svc.CreatePolicy(e.admin, in)
	if err != nil {
		t.Fatalf("create policy %s: %v", name, err)
	}
	return p
}

func (e *env) claims() map[string]any { return e.is.Claims(audience, "acme/app", "refs/heads/main") }

func exchangeReason(err error) string {
	var xe *federation.ExchangeError
	if errors.As(err, &xe) {
		return xe.Reason
	}
	return ""
}

func TestExchangeMintsScopedToken(t *testing.T) {
	e := newEnv(t)
	e.policy(t, "acme-app", nil)
	ctx := context.Background()
	res, err := e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if res.ServiceAccount != "ci-bot" || res.Policy != "acme-app" || res.TokenType != "Bearer" ||
		len(res.Scopes) != 1 || res.Scopes[0] != npmWrite || !strings.HasPrefix(res.Token, core.TokenPrefix) {
		t.Fatalf("result = %+v", res)
	}
	if ttl := time.Until(res.ExpiresAt); ttl > auth.DefaultFederatedTTL+time.Minute || ttl < auth.DefaultFederatedTTL-time.Minute {
		t.Errorf("token lives %s, want about the default %s", ttl, auth.DefaultFederatedTTL)
	}
	p, err := e.Svc.Authenticate(ctx, core.Credentials{Token: res.Token})
	if err != nil {
		t.Fatalf("authenticate minted token: %v", err)
	}
	authz := e.Svc.Authorizer()
	if err := authz.Authorize(ctx, p, core.ActionWrite, core.RepoResource("npm-hosted")); err != nil {
		t.Errorf("write to npm-hosted: %v", err)
	}
	if err := authz.Authorize(ctx, p, core.ActionWrite, core.RepoResource("maven-hosted")); !errors.Is(err, core.ErrForbidden) {
		t.Errorf("write to maven-hosted = %v, want forbidden (Publisher holds it, the policy scopes do not)", err)
	}
	cur, err := e.Svc.CurrentToken(core.WithPrincipal(ctx, p))
	if err != nil || cur == nil || cur.Kind != auth.TokenKindFederated {
		t.Fatalf("current token = %+v, %v; want kind federated", cur, err)
	}
	ev := e.Audit.Events(federation.AuditExchange)
	if len(ev) != 1 || ev[0].Outcome != core.OutcomeSuccess || ev[0].Actor != "ci-bot" ||
		ev[0].Details["policy"] != "acme-app" || ev[0].Details["issuer"] != e.is.URL ||
		ev[0].Details["subject"] != "repo:acme/app:ref:refs/heads/main" {
		t.Fatalf("audit = %+v", ev)
	}
	for k, v := range ev[0].Details {
		if s, ok := v.(string); ok && (strings.Contains(s, res.Token) || strings.Count(s, ".") == 2 && len(s) > 100) {
			t.Errorf("audit detail %s holds a token: %q", k, s)
		}
	}
}

func TestExchangeRefusals(t *testing.T) {
	e := newEnv(t)
	e.policy(t, "acme-app", nil)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	with := func(edit func(c map[string]any)) map[string]any {
		c := e.claims()
		edit(c)
		return c
	}
	now := time.Now()
	cases := []struct {
		name   string
		token  func() string
		reason string
	}{
		{"expired", func() string {
			return e.is.Sign(t, with(func(c map[string]any) { c["exp"] = now.Add(-2 * time.Minute).Unix() }), jose.RS256, "")
		}, federation.ReasonExpired},
		{"not valid yet", func() string {
			return e.is.Sign(t, with(func(c map[string]any) { c["nbf"] = now.Add(5 * time.Minute).Unix() }), jose.RS256, "")
		}, federation.ReasonNotYetValid},
		{"issued in the future", func() string {
			return e.is.Sign(t, with(func(c map[string]any) { c["iat"] = now.Add(5 * time.Minute).Unix() }), jose.RS256, "")
		}, federation.ReasonNotYetValid},
		{"no exp", func() string {
			return e.is.Sign(t, with(func(c map[string]any) { delete(c, "exp") }), jose.RS256, "")
		}, federation.ReasonMissingClaims},
		{"no jti", func() string {
			return e.is.Sign(t, with(func(c map[string]any) { delete(c, "jti") }), jose.RS256, "")
		}, federation.ReasonMissingClaims},
		{"wrong audience", func() string {
			return e.is.Sign(t, with(func(c map[string]any) { c["aud"] = "https://other.example" }), jose.RS256, "")
		}, federation.ReasonNoPolicy},
		{"claims do not match", func() string {
			return e.is.Sign(t, with(func(c map[string]any) { c["repository"] = "evil/app" }), jose.RS256, "")
		}, federation.ReasonNoPolicy},
		{"glob does not match", func() string {
			return e.is.Sign(t, with(func(c map[string]any) { c["ref"] = "refs/tags/v1" }), jose.RS256, "")
		}, federation.ReasonNoPolicy},
		{"iss of another issuer", func() string {
			return e.is.Sign(t, with(func(c map[string]any) { c["iss"] = "https://evil.example" }), jose.RS256, "")
		}, federation.ReasonUnknownIssuer},
		{"alg none", func() string { return federationtest.Unsigned(t, federationtest.RSAKeyID, e.claims()) }, federation.ReasonMalformed},
		{"HS256 keyed with the public key", func() string { return e.is.HS256WithPublicKey(t, e.claims()) }, federation.ReasonMalformed},
		{"RS512 is not allowed", func() string {
			return federationtest.SignWith(t, otherKey, jose.RS512, federationtest.RSAKeyID, e.claims())
		}, federation.ReasonMalformed},
		{"signed by an unpublished key", func() string {
			return federationtest.SignWith(t, otherKey, jose.RS256, federationtest.RSAKeyID, e.claims())
		}, federation.ReasonBadSignature},
		{"RSA token under the EC key id", func() string { return e.is.Sign(t, e.claims(), jose.RS256, federationtest.ECKeyID) },
			federation.ReasonUnknownKey},
		{"unknown key id", func() string { return e.is.Sign(t, e.claims(), jose.RS256, "nope") }, federation.ReasonUnknownKey},
		{"not a JWT", func() string { return "hello.world" }, federation.ReasonMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.Exchange(context.Background(), federation.ExchangeInput{Token: tc.token()})
			if got := exchangeReason(err); got != tc.reason {
				t.Fatalf("err = %v (reason %q), want reason %q", err, got, tc.reason)
			}
			if !errors.Is(err, core.ErrUnauthenticated) {
				t.Errorf("err = %v, want it to wrap ErrUnauthenticated", err)
			}
		})
	}
	ev := e.Audit.Events(federation.AuditExchange)
	if len(ev) != len(cases) {
		t.Fatalf("%d exchange audit events, want %d", len(ev), len(cases))
	}
	for _, x := range ev {
		if x.Outcome != core.OutcomeDenied || x.Details["reason"] == nil {
			t.Errorf("audit = %+v, want denied with a reason", x)
		}
	}
}

func TestExchangeUnknownIssuerIsNotContacted(t *testing.T) {
	e := newEnv(t)
	e.policy(t, "acme-app", nil)
	stranger := federationtest.New(t, false)
	_, err := e.svc.Exchange(context.Background(), federation.ExchangeInput{
		Token: stranger.Sign(t, stranger.Claims(audience, "acme/app", "refs/heads/main"), jose.RS256, "")})
	if exchangeReason(err) != federation.ReasonUnknownIssuer {
		t.Fatalf("err = %v, want unknown_issuer", err)
	}
	if n := stranger.DiscoveryHits.Load() + stranger.KeyHits.Load(); n != 0 {
		t.Fatalf("the unknown issuer got %d requests, want none", n)
	}
}

func TestExchangeES256(t *testing.T) {
	e := newEnv(t)
	e.policy(t, "acme-app", nil)
	if _, err := e.svc.Exchange(context.Background(), federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.ES256, "")}); err != nil {
		t.Fatalf("ES256 exchange: %v", err)
	}
}

func TestExchangeReplay(t *testing.T) {
	e := newEnv(t)
	e.policy(t, "acme-app", nil)
	tok := e.is.Sign(t, e.claims(), jose.RS256, "")
	if _, err := e.svc.Exchange(context.Background(), federation.ExchangeInput{Token: tok}); err != nil {
		t.Fatalf("first exchange: %v", err)
	}
	_, err := e.svc.Exchange(context.Background(), federation.ExchangeInput{Token: tok})
	if exchangeReason(err) != federation.ReasonReplayed || !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("second exchange = %v, want replayed", err)
	}
}

func TestExchangeKeyCache(t *testing.T) {
	e := newEnv(t)
	e.policy(t, "acme-app", nil)
	ctx := context.Background()
	for range 3 {
		if _, err := e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")}); err != nil {
			t.Fatal(err)
		}
	}
	if d, k := e.is.DiscoveryHits.Load(), e.is.KeyHits.Load(); d != 1 || k != 1 {
		t.Fatalf("discovery fetched %d times, keys %d times; want once each", d, k)
	}
	// A rotated key (new kid) is fetched on first sight.
	e.is.Rotate(t, "rsa-2")
	if _, err := e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")}); err != nil {
		t.Fatalf("exchange after key rotation: %v", err)
	}
	if k := e.is.KeyHits.Load(); k != 2 {
		t.Fatalf("keys fetched %d times, want 2 (refresh on unknown kid)", k)
	}
	// Made-up key ids do not make the server fetch again right away.
	for range 3 {
		_, err := e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "made-up")})
		if exchangeReason(err) != federation.ReasonUnknownKey {
			t.Fatalf("made-up kid = %v", err)
		}
	}
	if k := e.is.KeyHits.Load(); k != 2 {
		t.Fatalf("keys fetched %d times after unknown kids, want still 2", k)
	}
	// After the minimum interval an unknown kid fetches again.
	e.Clock.Advance(time.Minute)
	_, _ = e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "made-up")})
	if k := e.is.KeyHits.Load(); k != 3 {
		t.Fatalf("keys fetched %d times, want 3", k)
	}
}

func TestExchangeIssuerFetchUsesDialGuard(t *testing.T) {
	plain := federationtest.New(t, true)
	var dialled []netip.Addr
	e := newEnv(t, func(o *federation.Options) {
		o.HTTPClient, o.AllowHTTPIssuers = nil, true
		o.DialGuard = func(a netip.Addr) error {
			dialled = append(dialled, a)
			return errors.New("blocked by test guard")
		}
	})
	e.is = plain
	e.policy(t, "plain", nil)
	_, err := e.svc.Exchange(context.Background(), federation.ExchangeInput{Token: plain.Sign(t, e.claims(), jose.RS256, "")})
	if !errors.Is(err, core.ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
	if len(dialled) == 0 || plain.DiscoveryHits.Load() != 0 {
		t.Fatalf("guard saw %v, issuer got %d requests; want the guard to stop the dial", dialled, plain.DiscoveryHits.Load())
	}
}

func TestExchangeAmbiguousAndNamedPolicy(t *testing.T) {
	e := newEnv(t)
	e.policy(t, "a", nil)
	e.policy(t, "b", func(in *federation.PolicyInput) {
		in.Scopes = []core.Privilege{core.MustParsePrivilege("repo:npm-other:write")}
	})
	ctx := context.Background()
	_, err := e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")})
	if exchangeReason(err) != federation.ReasonAmbiguous || !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("err = %v, want ambiguous_policy (invalid)", err)
	}
	res, err := e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, ""), Policy: "b"})
	if err != nil || res.Policy != "b" || res.Scopes[0].String() != "repo:npm-other:write" {
		t.Fatalf("named policy = %+v, %v", res, err)
	}
}

func TestExchangeDisabledPolicyAndAccount(t *testing.T) {
	e := newEnv(t)
	e.policy(t, "off", func(in *federation.PolicyInput) { in.Enabled = new(false) })
	_, err := e.svc.Exchange(context.Background(), federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")})
	if exchangeReason(err) != federation.ReasonUnknownIssuer {
		t.Fatalf("disabled policy: err = %v, want unknown_issuer", err)
	}

	e.policy(t, "on", nil)
	bot, err := e.Svc.GetUserByName(e.admin, "ci-bot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.SetUserDisabled(e.admin, bot.ID, true); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Exchange(context.Background(), federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")})
	if exchangeReason(err) != federation.ReasonAccountRefused || !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("disabled account: err = %v, want service_account_refused", err)
	}
}

func TestExchangeRateLimit(t *testing.T) {
	e := newEnv(t, func(o *federation.Options) { o.RateLimit = auth.RateLimit{PerMinute: 1, Burst: 1} })
	e.policy(t, "acme-app", nil)
	ctx := core.WithRequestInfo(context.Background(), core.RequestInfo{RemoteIP: "203.0.113.7"})
	if _, err := e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")})
	if d, ok := core.RetryAfter(err); !errors.Is(err, core.ErrRateLimited) || !ok || d <= 0 {
		t.Fatalf("second exchange = %v, want rate limited with a wait", err)
	}
	other := core.WithRequestInfo(context.Background(), core.RequestInfo{RemoteIP: "198.51.100.9"})
	if _, err := e.svc.Exchange(other, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")}); err != nil {
		t.Fatalf("another address: %v", err)
	}
}

func TestFederatedTokenLimits(t *testing.T) {
	e := newEnv(t)
	e.policy(t, "acme-app", func(in *federation.PolicyInput) { in.MaxTTLSeconds = 120 })
	ctx := context.Background()
	res, err := e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExpiresIn > 120 || res.ExpiresIn < 110 {
		t.Errorf("expires_in = %d, want the policy's 120", res.ExpiresIn)
	}
	p, err := e.Svc.Authenticate(ctx, core.Credentials{Token: res.Token})
	if err != nil {
		t.Fatal(err)
	}
	as := core.WithPrincipal(ctx, p)
	_, _, err = e.Svc.CreateToken(as, auth.TokenInput{Name: "x", Scopes: []core.Privilege{npmWrite}, ExpiresAt: time.Now().Add(time.Minute)})
	if !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("create token with a federated token = %v, want forbidden", err)
	}
	page, err := e.Svc.ListTokens(as, auth.TokenQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("listed tokens = %+v, %v; federated tokens are not listed", page.Items, err)
	}
	// A registry login through it ends when it does.
	_, exp, err := e.Svc.Issue(ctx, p, core.TokenRequest{TTL: time.Hour})
	if err != nil || exp.After(res.ExpiresAt) {
		t.Fatalf("session token from federated token expires %s (%v), want no later than %s", exp, err, res.ExpiresAt)
	}
	e.Clock.Advance(3 * time.Minute)
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Token: res.Token}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("expired federated token = %v, want unauthenticated", err)
	}
}

func TestPolicyUpdateAndDeleteRevokeTokens(t *testing.T) {
	e := newEnv(t)
	e.policy(t, "acme-app", nil)
	ctx := context.Background()
	mint := func() string {
		t.Helper()
		res, err := e.svc.Exchange(ctx, federation.ExchangeInput{Token: e.is.Sign(t, e.claims(), jose.RS256, "")})
		if err != nil {
			t.Fatal(err)
		}
		return res.Token
	}
	tok := mint()
	p, err := e.svc.GetPolicy(e.admin, "acme-app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.UpdatePolicy(e.admin, "acme-app", federation.PolicyInput{Issuer: p.Issuer, Conditions: p.Conditions,
		ServiceAccount: p.ServiceAccount, Scopes: p.Scopes, Description: "changed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Token: tok}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("token after policy update = %v, want revoked", err)
	}
	tok = mint()
	if err := e.svc.DeletePolicy(e.admin, "acme-app"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Svc.Authenticate(ctx, core.Credentials{Token: tok}); !errors.Is(err, core.ErrUnauthenticated) {
		t.Fatalf("token after policy delete = %v, want gone", err)
	}
}

func TestPolicyManagementNeedsCoverage(t *testing.T) {
	e := newEnv(t)
	// A settings administrator who can only publish to npm-*.
	if _, err := e.Svc.CreateRole(e.admin, auth.RoleInput{Name: "npm-settings", Privileges: []core.Privilege{
		core.MustParsePrivilege("system:settings:admin"), core.MustParsePrivilege("repo:npm-*:write"),
	}}); err != nil {
		t.Fatal(err)
	}
	pw := "a long enough passphrase"
	if _, err := e.Svc.CreateUser(e.admin, auth.UserInput{Username: "ops", Kind: auth.KindUser, Password: &pw,
		Roles: []string{"npm-settings"}}); err != nil {
		t.Fatal(err)
	}
	ops := e.As(t, "ops", pw)
	reader := func() context.Context {
		if _, err := e.Svc.CreateUser(e.admin, auth.UserInput{Username: "viewer", Kind: auth.KindUser, Password: &pw,
			Roles: []string{"Reader"}}); err != nil {
			t.Fatal(err)
		}
		return e.As(t, "viewer", pw)
	}()
	input := func(scope string) federation.PolicyInput {
		return federation.PolicyInput{Name: "p-" + strings.NewReplacer(":", "-", "*", "x").Replace(scope), Issuer: e.is.URL,
			ServiceAccount: "ci-bot", Scopes: []core.Privilege{core.MustParsePrivilege(scope)},
			Conditions: []federation.Condition{{Claim: "repository", Values: []string{"acme/app"}}}}
	}
	cases := []struct {
		name string
		ctx  context.Context
		in   federation.PolicyInput
		want error
	}{
		{"covered scope", ops, input("repo:npm-hosted:write"), nil},
		{"scope wider than the editor", ops, input("repo:*:write"), core.ErrForbidden},
		{"system scope", ops, input("system:users:admin"), core.ErrForbidden},
		{"no settings admin", reader, input("repo:npm-x:read"), core.ErrForbidden},
		{"administrator", e.admin, input("repo:*:admin"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.CreatePolicy(tc.ctx, tc.in)
			if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("create = %v, want %v", err, tc.want)
			}
		})
	}
	// The narrower editor may not touch the administrator's wider policy.
	wide := input("repo:*:admin")
	wide.Description = "retarget"
	if _, err := e.svc.UpdatePolicy(ops, wide.Name, wide); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("update wider policy = %v, want forbidden", err)
	}
	if err := e.svc.DeletePolicy(ops, wide.Name); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("delete wider policy = %v, want forbidden", err)
	}
	if _, err := e.svc.ListPolicies(reader); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("list as reader = %v, want forbidden", err)
	}
}

func TestPolicyValidation(t *testing.T) {
	e := newEnv(t)
	base := func() federation.PolicyInput {
		return federation.PolicyInput{Name: "p", Issuer: e.is.URL, ServiceAccount: "ci-bot", Scopes: []core.Privilege{npmWrite},
			Conditions: []federation.Condition{{Claim: "repository", Values: []string{"acme/app"}}}}
	}
	pw := "a long enough passphrase"
	if _, err := e.Svc.CreateUser(e.admin, auth.UserInput{Username: "human", Kind: auth.KindUser, Password: &pw}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		edit func(in *federation.PolicyInput)
	}{
		{"http issuer", func(in *federation.PolicyInput) { in.Issuer = "http://ci.example.com" }},
		{"issuer with query", func(in *federation.PolicyInput) { in.Issuer = e.is.URL + "?x=1" }},
		{"bad name", func(in *federation.PolicyInput) { in.Name = "has space" }},
		{"no scopes", func(in *federation.PolicyInput) { in.Scopes = nil }},
		{"ttl above an hour", func(in *federation.PolicyInput) { in.MaxTTLSeconds = 3601 }},
		{"ttl below a minute", func(in *federation.PolicyInput) { in.MaxTTLSeconds = 30 }},
		{"no conditions", func(in *federation.PolicyInput) { in.Conditions = nil }},
		{"unknown account", func(in *federation.PolicyInput) { in.ServiceAccount = "nobody" }},
		{"a user, not a service account", func(in *federation.PolicyInput) { in.ServiceAccount = "human" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.edit(&in)
			if _, err := e.svc.CreatePolicy(e.admin, in); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("create = %v, want invalid", err)
			}
		})
	}
	p, err := e.svc.CreatePolicy(e.admin, base())
	if err != nil {
		t.Fatal(err)
	}
	if p.Audience != audience || p.MaxTTLSeconds != int(auth.DefaultFederatedTTL/time.Second) || !p.Enabled {
		t.Fatalf("defaults = %+v", p)
	}
	if _, err := e.svc.CreatePolicy(e.admin, base()); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("duplicate = %v, want already exists", err)
	}
}
