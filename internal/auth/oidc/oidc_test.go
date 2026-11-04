package oidc_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc/oidctest"
)

const redirectURL = "https://hangar.example.com" + oidc.CallbackPath

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func config(is *oidctest.Issuer) auth.OIDCConfig {
	cfg := auth.Config{OIDC: auth.OIDCConfig{
		Enabled: true, IssuerURL: is.URL, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret,
		AutoCreate: true,
	}}.WithDefaults()
	return cfg.OIDC
}

func newProvider(t *testing.T, cfg auth.OIDCConfig, c *clock) *oidc.Provider {
	t.Helper()
	opts := oidc.Options{RedirectURL: redirectURL, StateKey: []byte("0123456789abcdef0123456789abcdef"), RetryInterval: time.Second}
	if c != nil {
		opts.Now = c.Now
	}
	p, err := oidc.New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// authorize follows the provider URL like a browser would, up to the
// redirect back to Hangar, and returns that callback's query.
func authorize(t *testing.T, authURL string) url.Values {
	t.Helper()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: status %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != redirectURL {
		t.Fatalf("redirected to %s, want %s", got, redirectURL)
	}
	return loc.Query()
}

func user(sub string, groups ...any) map[string]any {
	return map[string]any{
		"sub": sub, "preferred_username": "jdoe", "email": "jdoe@example.com", "email_verified": true,
		"name": "Jane Doe", "groups": groups,
	}
}

func signIn(t *testing.T, p *oidc.Provider, is *oidctest.Issuer, redirect string) (*auth.ExternalIdentity, string, error) {
	t.Helper()
	authURL, cookie, err := p.Begin(context.Background(), redirect)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	q := authorize(t, authURL)
	return p.Complete(context.Background(), cookie, q.Get("state"), q.Get("code"))
}

func wantCode(t *testing.T, err error, code, reason string) {
	t.Helper()
	var oe *oidc.Error
	if !errors.As(err, &oe) || oe.Code != code || (reason != "" && oe.Reason != reason) {
		t.Fatalf("err = %v, want code %s reason %s", err, code, reason)
	}
}

func TestHappyPath(t *testing.T) {
	is := oidctest.New(t)
	is.Set(func(is *oidctest.Issuer) { is.User = user("u-1", "devs", "ops") })
	p := newProvider(t, config(is), nil)
	id, redirect, err := signIn(t, p, is, "/repositories?q=1")
	if err != nil {
		t.Fatal(err)
	}
	if id.Source != auth.SourceOIDC || id.Issuer != is.URL || id.Subject != "u-1" || id.Username != "jdoe" ||
		id.Email != "jdoe@example.com" || id.DisplayName != "Jane Doe" || !slices.Equal(id.Groups, []string{"devs", "ops"}) {
		t.Fatalf("identity = %+v", id)
	}
	if redirect != "/repositories?q=1" {
		t.Fatalf("redirect = %q", redirect)
	}
	if ok, err := p.Status(); !ok || err != nil {
		t.Fatalf("status = %v, %v", ok, err)
	}
}

func TestAuthURLUsesPKCEAndNonce(t *testing.T) {
	is := oidctest.New(t)
	p := newProvider(t, config(is), nil)
	authURL, _, err := p.Begin(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" ||
		q.Get("state") == "" || q.Get("redirect_uri") != redirectURL || q.Get("scope") != "openid profile email" {
		t.Fatalf("auth URL query = %v", q)
	}
}

func TestBadState(t *testing.T) {
	is := oidctest.New(t)
	is.Set(func(is *oidctest.Issuer) { is.User = user("u-1") })
	p := newProvider(t, config(is), nil)
	authURL, cookie, err := p.Begin(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	q := authorize(t, authURL)
	ctx := context.Background()

	_, _, err = p.Complete(ctx, cookie, "forged-state", q.Get("code"))
	wantCode(t, err, oidc.CodeState, "state_mismatch")
	_, _, err = p.Complete(ctx, "", q.Get("state"), q.Get("code"))
	wantCode(t, err, oidc.CodeState, "bad_state_cookie")
	_, _, err = p.Complete(ctx, cookie[:len(cookie)-2]+"AA", q.Get("state"), q.Get("code"))
	wantCode(t, err, oidc.CodeState, "bad_state_cookie")

	// A cookie sealed by another server (other key) is rejected.
	other := oidc.Options{RedirectURL: redirectURL, StateKey: []byte("another key of thirty-two bytes!")}
	p2, err := oidc.New(config(is), other)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = p2.Complete(ctx, cookie, q.Get("state"), q.Get("code"))
	wantCode(t, err, oidc.CodeState, "bad_state_cookie")
}

func TestStateExpires(t *testing.T) {
	is := oidctest.New(t)
	is.Set(func(is *oidctest.Issuer) { is.User = user("u-1") })
	c := &clock{t: time.Now()}
	p := newProvider(t, config(is), c)
	authURL, cookie, err := p.Begin(context.Background(), "/")
	if err != nil {
		t.Fatal(err)
	}
	q := authorize(t, authURL)
	c.Advance(oidc.StateTTL + time.Second)
	_, _, err = p.Complete(context.Background(), cookie, q.Get("state"), q.Get("code"))
	wantCode(t, err, oidc.CodeState, "state_expired")
}

func TestTokenChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(map[string]any)
		reason string
	}{
		"bad nonce":      {func(c map[string]any) { c["nonce"] = "replayed" }, "nonce_mismatch"},
		"missing nonce":  {func(c map[string]any) { delete(c, "nonce") }, "nonce_mismatch"},
		"wrong audience": {func(c map[string]any) { c["aud"] = "another-client" }, "id_token_invalid"},
		"wrong issuer":   {func(c map[string]any) { c["iss"] = "https://evil.example.com" }, "id_token_invalid"},
		"expired":        {func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, "id_token_expired"},
	} {
		t.Run(name, func(t *testing.T) {
			is := oidctest.New(t)
			is.Set(func(is *oidctest.Issuer) { is.User, is.Mutate = user("u-1"), tc.mutate })
			p := newProvider(t, config(is), nil)
			_, _, err := signIn(t, p, is, "/")
			wantCode(t, err, oidc.CodeFailed, tc.reason)
		})
	}
}

func TestClientAuthentication(t *testing.T) {
	is := oidctest.New(t)
	is.Set(func(is *oidctest.Issuer) { is.User = user("u-1") })
	cfg := config(is)
	for _, method := range []string{"basic", "post"} {
		cfg.ClientAuth = method
		if _, _, err := signIn(t, newProvider(t, cfg, nil), is, "/"); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var got, scopes string
		is.Set(func(is *oidctest.Issuer) { got, scopes = is.ClientAuth, is.Scopes })
		if got != method {
			t.Fatalf("client_auth %s: issuer saw %s", method, got)
		}
		// groups is never requested as a scope: some providers reject it.
		if scopes != "openid profile email" {
			t.Fatalf("scopes = %q", scopes)
		}
	}
}

func TestWrongClientSecret(t *testing.T) {
	is := oidctest.New(t)
	is.Set(func(is *oidctest.Issuer) { is.User = user("u-1") })
	cfg := config(is)
	cfg.ClientSecret = "wrong"
	_, _, err := signIn(t, newProvider(t, cfg, nil), is, "/")
	wantCode(t, err, oidc.CodeFailed, "code_exchange")
}

func TestUserinfoCompletesClaims(t *testing.T) {
	is := oidctest.New(t)
	is.Set(func(is *oidctest.Issuer) {
		is.User = user("u-1", "admins")
		is.IDTokenOmits = []string{"groups"}
	})
	p := newProvider(t, config(is), nil)
	id, _, err := signIn(t, p, is, "/")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(id.Groups, []string{"admins"}) {
		t.Fatalf("groups = %v", id.Groups)
	}

	// Userinfo for another subject is refused.
	is.Set(func(is *oidctest.Issuer) {
		is.UserInfo = map[string]any{"sub": "someone-else", "groups": []any{"admins"}}
	})
	_, _, err = signIn(t, p, is, "/")
	wantCode(t, err, oidc.CodeFailed, "userinfo_subject_mismatch")
}

func TestClaimMapping(t *testing.T) {
	is := oidctest.New(t)
	is.Set(func(is *oidctest.Issuer) {
		is.User = map[string]any{
			"sub": "u-2", "upn": "jsmith", "email": "jsmith@example.com", "email_verified": false,
			"realm_access": map[string]any{"roles": []any{"hangar-admins", "offline_access"}},
		}
	})
	cfg := config(is)
	cfg.UsernameClaim, cfg.GroupsClaim = "upn", "realm_access.roles"
	id, _, err := signIn(t, newProvider(t, cfg, nil), is, "/")
	if err != nil {
		t.Fatal(err)
	}
	if id.Username != "jsmith" || id.Email != "" || !slices.Equal(id.Groups, []string{"hangar-admins", "offline_access"}) {
		t.Fatalf("identity = %+v (unverified email must be dropped)", id)
	}
}

func TestDiscoveryRetries(t *testing.T) {
	is := oidctest.New(t)
	is.Set(func(is *oidctest.Issuer) { is.Down, is.User = true, user("u-1") })
	c := &clock{t: time.Now()}
	p := newProvider(t, config(is), c)
	p.Warm(context.Background())
	if ok, err := p.Status(); ok || err == nil {
		t.Fatalf("status while down = %v, %v", ok, err)
	}
	_, _, err := p.Begin(context.Background(), "/")
	wantCode(t, err, oidc.CodeUnavailable, "discovery")

	is.Set(func(is *oidctest.Issuer) { is.Down = false })
	// Within the retry interval the cached failure is returned.
	_, _, err = p.Begin(context.Background(), "/")
	wantCode(t, err, oidc.CodeUnavailable, "discovery")
	c.Advance(2 * time.Second)
	if _, _, err := signIn(t, p, is, "/"); err != nil {
		t.Fatalf("after recovery: %v", err)
	}
}

func TestSafeRedirect(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/":                      "/",
		"/repositories?x=1#top":  "/repositories?x=1#top",
		"//evil.example.com":     "/",
		"/\\evil.example.com":    "/",
		"https://evil.example":   "/",
		"javascript:alert(1)":    "/",
		"repositories":           "/",
		"/ok\r\nSet-Cookie: x=1": "/",
	} {
		if got := oidc.SafeRedirect(in); got != want {
			t.Errorf("SafeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveStateKey(t *testing.T) {
	a, err := oidc.DeriveStateKey([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil || len(a) != 32 {
		t.Fatalf("key = %x, %v", a, err)
	}
	b, _ := oidc.DeriveStateKey([]byte("0123456789abcdef0123456789abcdef"))
	c, _ := oidc.DeriveStateKey([]byte("fedcba9876543210fedcba9876543210"))
	if string(a) != string(b) || string(a) == string(c) {
		t.Fatal("derivation is not deterministic per secret")
	}
}
