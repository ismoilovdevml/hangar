// Package oidc implements single sign-on with an OpenID Connect provider:
// the authorization code flow with PKCE, ID token verification (signature,
// issuer, audience, expiry, nonce), the mapping of claims to an
// auth.ExternalIdentity and RP-Initiated Logout (the provider's
// end_session_endpoint). Back-channel and front-channel logout are not
// implemented: a session ended at the provider alone keeps its Hangar
// session until that expires or the user signs out of Hangar.
//
// The state, nonce, PKCE verifier and post-login redirect travel in a short
// lived cookie encrypted with AES-256-GCM, so any node of a cluster that
// shares the key can finish a sign-in another node started. Provider
// discovery is lazy and retried, so an identity provider that is down when
// Hangar starts does not prevent the start.
package oidc

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ismoilovdevml/hangar/internal/auth"
)

// Paths of the sign-in endpoints, relative to the server root.
const (
	StartPath    = "/api/v1/auth/oidc/start"
	CallbackPath = "/api/v1/auth/oidc/callback"
	// CookiePath scopes the state cookie to the two endpoints.
	CookiePath = "/api/v1/auth/oidc"
	// CookieName is the state cookie.
	CookieName = "hangar_oidc_state"
	// StateTTL is how long a started sign-in may take.
	StateTTL = 10 * time.Minute
)

// Error codes, passed to the login page as ?error=<code>.
const (
	CodeUnavailable = "sso_unavailable" // discovery failed
	CodeState       = "sso_state"       // missing, expired or mismatched state
	CodeFailed      = "sso_failed"      // code exchange or token verification failed
	CodeDenied      = "access_denied"   // the provider refused or the user cancelled
)

// Error is a failed sign-in step. Reason is for the audit log, Code for the
// user.
type Error struct {
	Code   string
	Reason string
	Err    error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return "oidc: " + e.Reason
	}
	return "oidc: " + e.Reason + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

func fail(code, reason string, err error) *Error { return &Error{Code: code, Reason: reason, Err: err} }

// Options are the collaborators of a Provider.
type Options struct {
	// RedirectURL is server.base_url + CallbackPath.
	RedirectURL string
	// PostLogoutRedirectURL is where the provider sends the browser after
	// RP-initiated logout: server.base_url + "/login". Empty: the provider
	// shows its own page.
	PostLogoutRedirectURL string
	// StateKey encrypts the state cookie; at least 32 bytes, the same on
	// every node. Nil: a random per-process key (single node only).
	StateKey []byte
	// HTTPClient replaces the client built from the TLS settings (tests).
	HTTPClient *http.Client
	// RetryInterval is the least time between discovery attempts after a
	// failure (default 15s).
	RetryInterval time.Duration
	Logger        *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Provider is a configured OIDC provider.
type Provider struct {
	cfg    auth.OIDCConfig
	opts   Options
	client *http.Client
	aead   cipher.AEAD
	log    *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	provider *gooidc.Provider
	verifier *gooidc.IDTokenVerifier
	oauth    *oauth2.Config
	// endSession is the end_session_endpoint of the discovery document ("" if none).
	endSession string
	lastErr    error
	lastTry  time.Time
}

// DeriveStateKey derives the state cookie key from the server secret key
// (security.secret_key), so every node agrees without another setting.
func DeriveStateKey(secret []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, secret, nil, "hangar oidc state v1", 32)
}

// New returns a provider for cfg (defaults applied by auth.Config). It does
// not contact the provider; see Warm.
func New(cfg auth.OIDCConfig, opts Options) (*Provider, error) {
	if opts.RedirectURL == "" {
		return nil, errors.New("oidc: redirect URL is required")
	}
	p := &Provider{cfg: cfg, opts: opts, client: opts.HTTPClient, log: opts.Logger, now: opts.Now}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.opts.RetryInterval <= 0 {
		p.opts.RetryInterval = 15 * time.Second
	}
	key := opts.StateKey
	if len(key) == 0 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
	} else if len(key) < 32 {
		return nil, errors.New("oidc: state key must be at least 32 bytes")
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}
	if p.aead, err = cipher.NewGCM(block); err != nil {
		return nil, err
	}
	if p.client == nil {
		if p.client, err = NewHTTPClient(cfg); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// NewHTTPClient returns a client for requests to the provider: TLS 1.2 or
// newer, the system roots plus ca_file and the CA bundle of cfg, and the
// configured timeout.
func NewHTTPClient(cfg auth.OIDCConfig) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // G402: opt-in for test providers, documented as unsafe
	if cfg.CAFile != "" || cfg.CAPEM != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("auth.oidc.ca_file: %w", err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("auth.oidc.ca_file %s: no PEM certificates found", cfg.CAFile)
			}
		}
		if cfg.CAPEM != "" && !pool.AppendCertsFromPEM([]byte(cfg.CAPEM)) {
			return nil, errors.New("oidc: the CA bundle has no PEM certificates")
		}
		tc.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tc
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

// Config returns the settings the provider was built from.
func (p *Provider) Config() auth.OIDCConfig { return p.cfg }

// DisplayName labels the sign-in button.
func (p *Provider) DisplayName() string { return p.cfg.DisplayName }

// Warm attempts discovery once (in the background at start-up) and logs the
// outcome; sign-ins retry on their own.
func (p *Provider) Warm(ctx context.Context) {
	if _, err := p.discover(ctx); err != nil {
		p.log.WarnContext(ctx, "auth.oidc: provider discovery failed; sign-in retries it", "issuer", p.cfg.IssuerURL, "error", err)
		return
	}
	p.log.InfoContext(ctx, "auth.oidc: provider discovered", "issuer", p.cfg.IssuerURL)
}

// Status reports whether discovery has succeeded, and the last error.
func (p *Provider) Status() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.provider != nil, p.lastErr
}

func (p *Provider) discover(ctx context.Context) (*oauth2.Config, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return p.oauth, nil
	}
	if p.lastErr != nil && p.now().Sub(p.lastTry) < p.opts.RetryInterval {
		return nil, p.lastErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.lastTry = p.now()
	// The provider keeps this context (without its deadline) to refresh
	// signing keys later, so it must not carry request-scoped values.
	dctx, cancel := context.WithTimeout(gooidc.ClientContext(context.Background(), p.client), p.cfg.Timeout)
	defer cancel()
	prov, err := gooidc.NewProvider(dctx, p.cfg.IssuerURL)
	if err != nil {
		p.lastErr = err
		return nil, err
	}
	var extra struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = prov.Claims(&extra) // the document already decoded once
	if u, err := url.Parse(extra.EndSession); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
		p.endSession = extra.EndSession
	}
	p.provider, p.lastErr = prov, nil
	p.verifier = prov.Verifier(&gooidc.Config{ClientID: p.cfg.ClientID, Now: p.now})
	ep := prov.Endpoint()
	ep.AuthStyle = oauth2.AuthStyleInHeader // client_secret_basic
	if p.cfg.ClientAuth == "post" {
		ep.AuthStyle = oauth2.AuthStyleInParams // client_secret_post
	}
	p.oauth = &oauth2.Config{
		ClientID: p.cfg.ClientID, ClientSecret: p.cfg.ClientSecret,
		Endpoint: ep, RedirectURL: p.opts.RedirectURL, Scopes: p.cfg.Scopes,
	}
	return p.oauth, nil
}

// state is the encrypted content of the state cookie.
type state struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Redirect string `json:"r"`
	Expires  int64  `json:"e"`
}

const cookieAAD = "hangar oidc state cookie v1"

// Begin starts a sign-in that returns to redirect (a same-origin path; any
// other value becomes "/"). It returns the provider URL to send the browser
// to and the state cookie value.
func (p *Provider) Begin(ctx context.Context, redirect string) (authURL, cookie string, err error) {
	oc, err := p.discover(ctx)
	if err != nil {
		return "", "", fail(CodeUnavailable, "discovery", err)
	}
	st := state{Verifier: oauth2.GenerateVerifier(), Redirect: SafeRedirect(redirect), Expires: p.now().Add(StateTTL).Unix()}
	if st.State, err = random(); err != nil {
		return "", "", err
	}
	if st.Nonce, err = random(); err != nil {
		return "", "", err
	}
	if cookie, err = p.seal(st); err != nil {
		return "", "", err
	}
	return oc.AuthCodeURL(st.State, gooidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), cookie, nil
}

// Result is a finished sign-in.
type Result struct {
	Identity *auth.ExternalIdentity
	// Redirect is the same-origin path given to Begin.
	Redirect string
	// IDToken is the raw ID token, the id_token_hint of a later logout.
	IDToken string
}

// Complete finishes a sign-in: it checks the callback's state against the
// cookie, exchanges the code (with the PKCE verifier), verifies the ID
// token and its nonce, and maps the claims (completed from the userinfo
// endpoint when the ID token lacks some).
func (p *Provider) Complete(ctx context.Context, cookie, gotState, code string) (*Result, error) {
	st, err := p.open(cookie)
	if err != nil {
		return nil, fail(CodeState, "bad_state_cookie", err)
	}
	if p.now().Unix() > st.Expires {
		return nil, fail(CodeState, "state_expired", nil)
	}
	if gotState == "" || subtle.ConstantTimeCompare([]byte(gotState), []byte(st.State)) != 1 {
		return nil, fail(CodeState, "state_mismatch", nil)
	}
	if code == "" {
		return nil, fail(CodeFailed, "missing_code", nil)
	}
	oc, err := p.discover(ctx)
	if err != nil {
		return nil, fail(CodeUnavailable, "discovery", err)
	}
	ctx = gooidc.ClientContext(ctx, p.client)
	tok, err := oc.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		return nil, fail(CodeFailed, "code_exchange", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, fail(CodeFailed, "no_id_token", nil)
	}
	p.mu.Lock()
	verifier, prov := p.verifier, p.provider
	p.mu.Unlock()
	idt, err := verifier.Verify(ctx, raw)
	if err != nil {
		reason := "id_token_invalid"
		var expired *gooidc.TokenExpiredError
		if errors.As(err, &expired) {
			reason = "id_token_expired"
		}
		return nil, fail(CodeFailed, reason, err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(st.Nonce)) != 1 {
		return nil, fail(CodeFailed, "nonce_mismatch", nil)
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		return nil, fail(CodeFailed, "id_token_claims", err)
	}
	if p.incomplete(claims) && prov.UserInfoEndpoint() != "" {
		info, err := prov.UserInfo(ctx, oauth2.StaticTokenSource(tok))
		if err != nil {
			return nil, fail(CodeFailed, "userinfo", err)
		}
		if info.Subject != idt.Subject {
			return nil, fail(CodeFailed, "userinfo_subject_mismatch", nil)
		}
		extra := map[string]any{}
		if err := info.Claims(&extra); err != nil {
			return nil, fail(CodeFailed, "userinfo_claims", err)
		}
		for k, v := range extra {
			if _, ok := claims[k]; !ok {
				claims[k] = v
			}
		}
	}
	id := &auth.ExternalIdentity{
		Source:      auth.SourceOIDC,
		Issuer:      idt.Issuer,
		Subject:     idt.Subject,
		Username:    claimString(claims, p.cfg.UsernameClaim),
		Email:       claimString(claims, p.cfg.EmailClaim),
		DisplayName: claimString(claims, p.cfg.NameClaim),
		Groups:      claimStrings(claims, p.cfg.GroupsClaim),
	}
	// An address the provider says it has not verified must not be stored:
	// emails are unique, so it could block the real owner.
	if v, ok := claims["email_verified"].(bool); ok && !v {
		id.Email = ""
	}
	return &Result{Identity: id, Redirect: st.Redirect, IDToken: raw}, nil
}

// EndSessionEndpoint is the provider's end_session_endpoint ("" when it
// has none or discovery has not succeeded yet).
func (p *Provider) EndSessionEndpoint() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.endSession
}

// LogoutURL returns where to send the browser to end the provider session
// too (OpenID Connect RP-Initiated Logout 1.0): the end_session_endpoint
// with id_token_hint (when idToken is known), client_id and
// post_logout_redirect_uri. It returns "" when auth.oidc.logout is local or
// the provider has no end_session_endpoint (or cannot be reached): signing
// out then ends the Hangar session only.
func (p *Provider) LogoutURL(ctx context.Context, idToken string) string {
	if p.cfg.Logout == auth.OIDCLogoutLocal {
		return ""
	}
	if _, err := p.discover(ctx); err != nil {
		p.log.WarnContext(ctx, "auth.oidc: provider unreachable; signing out of Hangar only", "error", err)
		return ""
	}
	endpoint := p.EndSessionEndpoint()
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	q := u.Query()
	if idToken != "" {
		q.Set("id_token_hint", idToken)
	}
	q.Set("client_id", p.cfg.ClientID)
	if p.opts.PostLogoutRedirectURL != "" {
		q.Set("post_logout_redirect_uri", p.opts.PostLogoutRedirectURL)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// incomplete reports whether claims lack the username or groups claim.
func (p *Provider) incomplete(claims map[string]any) bool {
	return claimValue(claims, p.cfg.UsernameClaim) == nil || claimValue(claims, p.cfg.GroupsClaim) == nil
}

func (p *Provider) seal(st state) (string, error) {
	plain, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(p.aead.Seal(nonce, nonce, plain, []byte(cookieAAD))), nil
}

func (p *Provider) open(cookie string) (state, error) {
	var st state
	if cookie == "" || len(cookie) > 4096 {
		return st, errors.New("missing state cookie")
	}
	b, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil || len(b) < p.aead.NonceSize() {
		return st, errors.New("malformed state cookie")
	}
	ns := p.aead.NonceSize()
	plain, err := p.aead.Open(nil, b[:ns], b[ns:], []byte(cookieAAD))
	if err != nil {
		return st, errors.New("state cookie was not issued by this server")
	}
	if err := json.Unmarshal(plain, &st); err != nil {
		return st, err
	}
	return st, nil
}

func random() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// SafeRedirect returns r when it is a same-origin path ("/repos?x=1"), else "/".
func SafeRedirect(r string) string {
	if r == "" || len(r) > 2048 || !strings.HasPrefix(r, "/") || strings.HasPrefix(r, "//") || strings.ContainsAny(r, "\\\r\n\t") {
		return "/"
	}
	u, err := url.Parse(r)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return r
}

// claimValue looks a claim up by name, then as a dotted path into nested
// objects ("realm_access.roles").
func claimValue(claims map[string]any, name string) any {
	if name == "" {
		return nil
	}
	if v, ok := claims[name]; ok {
		return v
	}
	var cur any = claims
	for part := range strings.SplitSeq(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = m[part]; !ok {
			return nil
		}
	}
	return cur
}

func claimString(claims map[string]any, name string) string {
	switch v := claimValue(claims, name).(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return fmt.Sprint(v)
	}
	return ""
}

// claimStrings reads a list of strings, or a single string, or a
// space/comma separated string.
func claimStrings(claims map[string]any, name string) []string {
	var out []string
	switch v := claimValue(claims, name).(type) {
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		for s := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			out = append(out, s)
		}
	}
	if len(out) > 1000 {
		out = out[:1000]
	}
	return out
}
