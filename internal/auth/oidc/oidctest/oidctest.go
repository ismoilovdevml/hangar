// Package oidctest is an in-process OpenID Connect provider for tests:
// discovery, JWKS, an authorization endpoint that signs the user in at once,
// a token endpoint that checks the client secret and PKCE, and userinfo.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	upstream "github.com/coreos/go-oidc/v3/oidc/oidctest"
)

// Client credentials the issuer accepts.
const (
	ClientID     = "hangar"
	ClientSecret = "test-client-secret"
)

// Issuer is the fake provider. Set User (and the knobs) before a sign-in.
type Issuer struct {
	*httptest.Server
	key  *rsa.PrivateKey
	keys *upstream.Server

	mu sync.Mutex
	// User holds the claims of the next sign-in ("sub" is required).
	User map[string]any
	// UserInfo, when set, is served by the userinfo endpoint instead of User.
	UserInfo map[string]any
	// IDTokenOmits lists claims left out of the ID token (still in userinfo).
	IDTokenOmits []string
	// Mutate edits the ID token claims just before signing (aud, exp, nonce).
	Mutate func(claims map[string]any)
	// Deny makes the authorization endpoint answer error=access_denied.
	Deny bool
	// Down makes discovery fail with 503.
	Down bool
	// ClientAuth is how the last token request authenticated the client:
	// "basic" or "post".
	ClientAuth string
	// Scopes are the scopes of the last authorization request.
	Scopes string

	codes map[string]grant
}

type grant struct {
	nonce, challenge, redirect string
	claims                     map[string]any
}

// New starts an issuer at http://127.0.0.1:port (closed at cleanup).
func New(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &Issuer{key: key, codes: map[string]grant{}}
	is.keys = &upstream.Server{PublicKeys: []upstream.PublicKey{{PublicKey: key.Public(), KeyID: "k1", Algorithm: gooidc.RS256}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", is.discovery)
	mux.Handle("GET /keys", is.keys)
	mux.HandleFunc("GET /authorize", is.authorize)
	mux.HandleFunc("POST /token", is.token)
	mux.HandleFunc("GET /userinfo", is.userinfo)
	is.Server = httptest.NewServer(mux)
	is.keys.SetIssuer(is.URL)
	t.Cleanup(is.Close)
	return is
}

// Set replaces the user of the next sign-in.
func (is *Issuer) Set(fn func(is *Issuer)) {
	is.mu.Lock()
	defer is.mu.Unlock()
	fn(is)
}

func (is *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	is.mu.Lock()
	down := is.Down
	is.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{
		"issuer":                                is.URL,
		"authorization_endpoint":                is.URL + "/authorize",
		"token_endpoint":                        is.URL + "/token",
		"userinfo_endpoint":                     is.URL + "/userinfo",
		"jwks_uri":                              is.URL + "/keys",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (is *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("client_id") != ClientID || q.Get("response_type") != "code" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	rq := redirect.Query()
	rq.Set("state", q.Get("state"))
	is.mu.Lock()
	is.Scopes = q.Get("scope")
	deny := is.Deny
	g := grant{nonce: q.Get("nonce"), redirect: redirect.String(), claims: maps.Clone(is.User)}
	if q.Get("code_challenge_method") == "S256" {
		g.challenge = q.Get("code_challenge")
	}
	code := rand.Text()
	if !deny {
		is.codes[code] = g
	}
	is.mu.Unlock()
	if deny {
		rq.Set("error", "access_denied")
	} else {
		rq.Set("code", code)
	}
	redirect.RawQuery = rq.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound) //nolint:gosec // G710: test issuer
}

func (is *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	method := "basic"
	id, secret, ok := r.BasicAuth()
	if !ok {
		method = "post"
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	is.mu.Lock()
	is.ClientAuth = method
	is.mu.Unlock()
	if id != ClientID || secret != ClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"error": "invalid_client"})
		return
	}
	is.mu.Lock()
	g, found := is.codes[r.PostForm.Get("code")]
	delete(is.codes, r.PostForm.Get("code"))
	omit, mutate := is.IDTokenOmits, is.Mutate
	is.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || r.PostForm.Get("redirect_uri") != g.redirect || g.challenge == "" ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]any{"error": "invalid_grant"})
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": is.URL, "aud": ClientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": g.nonce,
	}
	for k, v := range g.claims {
		claims[k] = v
	}
	for _, k := range omit {
		delete(claims, k)
	}
	if mutate != nil {
		mutate(claims)
	}
	raw, _ := json.Marshal(claims)
	access := "at-" + rand.Text()
	is.mu.Lock()
	is.codes[access] = g // userinfo looks the grant up by access token
	is.mu.Unlock()
	writeJSON(w, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 300,
		"id_token": upstream.SignIDToken(is.key, "k1", gooidc.RS256, string(raw)),
	})
}

func (is *Issuer) userinfo(w http.ResponseWriter, r *http.Request) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	is.mu.Lock()
	g, ok := is.codes[h[len(prefix):]]
	info := is.UserInfo
	is.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if info == nil {
		info = g.claims
	}
	writeJSON(w, info)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
