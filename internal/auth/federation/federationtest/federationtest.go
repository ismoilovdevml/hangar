// Package federationtest is an in-process OpenID Connect issuer for tests
// of the CI OIDC token exchange: it serves discovery and a key set (an RSA
// and a P-256 key) and signs ID tokens the way a CI platform does.
package federationtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Key ids of the published keys.
const (
	RSAKeyID = "rsa-1"
	ECKeyID  = "ec-1"
)

// Issuer is the fake CI platform.
type Issuer struct {
	*httptest.Server

	rsaKey *rsa.PrivateKey
	ecKey  *ecdsa.PrivateKey

	mu        sync.Mutex
	published []jose.JSONWebKey
	// DiscoveryHits and KeyHits count requests to the two documents.
	DiscoveryHits atomic.Int64
	KeyHits       atomic.Int64
}

// New starts an issuer over TLS (its URL is https://127.0.0.1:port; use
// Client, which trusts it) or, with plain, over http. It is closed at
// cleanup.
func New(t testing.TB, plain bool) *Issuer {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	is := &Issuer{rsaKey: rk, ecKey: ek}
	is.published = []jose.JSONWebKey{
		{Key: rk.Public(), KeyID: RSAKeyID, Algorithm: string(jose.RS256), Use: "sig"},
		{Key: ek.Public(), KeyID: ECKeyID, Algorithm: string(jose.ES256), Use: "sig"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		is.DiscoveryHits.Add(1)
		writeJSON(w, map[string]any{"issuer": is.URL, "jwks_uri": is.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256", "ES256"}})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		is.KeyHits.Add(1)
		is.mu.Lock()
		set := jose.JSONWebKeySet{Keys: append([]jose.JSONWebKey(nil), is.published...)}
		is.mu.Unlock()
		writeJSON(w, set)
	})
	if plain {
		is.Server = httptest.NewServer(mux)
	} else {
		is.Server = httptest.NewTLSServer(mux)
	}
	t.Cleanup(is.Close)
	return is
}

// Rotate publishes a new RSA key under kid (replacing the published RSA
// key) and signs RS256 tokens with it from now on.
func (is *Issuer) Rotate(t testing.TB, kid string) {
	t.Helper()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is.mu.Lock()
	defer is.mu.Unlock()
	is.rsaKey = rk
	is.published[0] = jose.JSONWebKey{Key: rk.Public(), KeyID: kid, Algorithm: string(jose.RS256), Use: "sig"}
}

// Claims returns typical GitHub Actions claims for repository on ref, for
// audience, valid from now for five minutes.
func (is *Issuer) Claims(audience, repository, ref string) map[string]any {
	now := time.Now()
	owner := repository
	for i := range repository {
		if repository[i] == '/' {
			owner = repository[:i]
			break
		}
	}
	return map[string]any{
		"iss": is.URL, "aud": audience, "sub": "repo:" + repository + ":ref:" + ref,
		"jti": randomID(), "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"repository": repository, "repository_owner": owner, "ref": ref, "ref_type": "branch",
		"workflow_ref": repository + "/.github/workflows/release.yml@" + ref, "environment": "production",
	}
}

// Sign signs claims with the issuer's key for alg (RS256 or ES256) under
// kid (the published id of that key when empty).
func (is *Issuer) Sign(t testing.TB, claims map[string]any, alg jose.SignatureAlgorithm, kid string) string {
	t.Helper()
	is.mu.Lock()
	var key any = is.rsaKey
	def := is.published[0].KeyID
	is.mu.Unlock()
	if alg == jose.ES256 {
		key, def = is.ecKey, ECKeyID
	}
	if kid == "" {
		kid = def
	}
	return SignWith(t, key, alg, kid, claims)
}

// SignWith signs claims with any key (a key the issuer does not publish,
// to test signature checks).
func SignWith(t testing.TB, key any, alg jose.SignatureAlgorithm, kid string, claims map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := sig.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Unsigned returns claims as a token with header alg "none" and an empty
// signature.
func Unsigned(t testing.TB, kid string, claims map[string]any) string {
	t.Helper()
	return unsafeToken(t, map[string]any{"alg": "none", "kid": kid, "typ": "JWT"}, claims, nil)
}

// HS256WithPublicKey is the algorithm confusion attack: the token is
// "signed" with HMAC-SHA256 using the DER of the issuer's published RSA
// public key as the secret, which a verifier that trusts the header's alg
// would accept.
func (is *Issuer) HS256WithPublicKey(t testing.TB, claims map[string]any) string {
	t.Helper()
	is.mu.Lock()
	pub := is.rsaKey.Public()
	kid := is.published[0].KeyID
	is.mu.Unlock()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return unsafeToken(t, map[string]any{"alg": "HS256", "kid": kid, "typ": "JWT"}, claims, der)
}

func unsafeToken(t testing.TB, header, claims map[string]any, hmacKey []byte) string {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	c, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	if hmacKey == nil {
		return signing + "."
	}
	m := hmac.New(sha256.New, hmacKey)
	m.Write([]byte(signing))
	return signing + "." + enc.EncodeToString(m.Sum(nil))
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
