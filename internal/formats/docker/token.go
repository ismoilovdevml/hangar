package docker

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Registry bearer tokens are compact JWTs signed with HMAC-SHA256 (HS256)
// using the server key (Options.TokenKey / HANGAR_DOCKER_TOKEN_KEY). They are
// verified locally on every /v2/ request without a database round trip and
// are only accepted on /v2/. Every node of a cluster must share the key.

const tokenIssuer = "hangar"

var errBadToken = errors.New("invalid registry token")

// accessEntry is one granted resource in the "access" claim, as defined by the
// distribution token authentication specification.
type accessEntry struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// tokenPrincipal is the identity the token was issued to.
type tokenPrincipal struct {
	ID      string             `json:"id,omitempty"`
	Name    string             `json:"name"`
	Kind    core.PrincipalKind `json:"kind"`
	Source  string             `json:"src,omitempty"`
	Roles   []string           `json:"roles,omitempty"`
	TokenID string             `json:"tid,omitempty"`
	// Scopes are the scopes of the credentials used at the token endpoint
	// (nil = unrestricted). Kept to filter the catalog and to tell
	// insufficient_scope from a real denial.
	Scopes []core.Privilege `json:"scopes"`
}

type tokenClaims struct {
	Issuer    string        `json:"iss"`
	Subject   string        `json:"sub"`
	Audience  string        `json:"aud"`
	ExpiresAt int64         `json:"exp"`
	NotBefore int64         `json:"nbf"`
	IssuedAt  int64         `json:"iat"`
	ID        string        `json:"jti"`
	Access    []accessEntry `json:"access"`
	// Privileges are the Hangar privileges the access list was granted as
	// (including the write member of a group); they become Principal.Scopes.
	Privileges []core.Privilege `json:"privs"`
	Catalog    bool             `json:"catalog,omitempty"`
	Principal  tokenPrincipal   `json:"hgr"`
}

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// signer signs and verifies registry tokens and upload state.
type signer struct {
	key []byte
}

func (s signer) mac(parts ...string) []byte {
	m := hmac.New(sha256.New, s.key)
	for i, p := range parts {
		if i > 0 {
			m.Write([]byte{0})
		}
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

func (s signer) sign(c *tokenClaims) (string, error) {
	body, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode token: %w", err)
	}
	unsigned := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(body)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(s.mac(unsigned)), nil
}

// looksLikeJWT reports whether a bearer token is one of ours (as opposed to a
// Hangar personal access token, which the core authenticator handles).
func looksLikeJWT(tok string) bool {
	return strings.HasPrefix(tok, "eyJ") && strings.Count(tok, ".") == 2
}

func (s signer) verify(tok, audience string, now time.Time) (*tokenClaims, error) {
	h, rest, ok := strings.Cut(tok, ".")
	if !ok || h != jwtHeader {
		return nil, errBadToken
	}
	payload, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return nil, errBadToken
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(h+"."+payload)) {
		return nil, errBadToken
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, errBadToken
	}
	var c tokenClaims
	if err := json.Unmarshal(body, &c); err != nil {
		return nil, errBadToken
	}
	t := now.Unix()
	if c.Issuer != tokenIssuer || c.Audience != audience || t >= c.ExpiresAt || t+60 < c.NotBefore {
		return nil, errBadToken
	}
	return &c, nil
}

// principal rebuilds the request principal from verified claims. Token scopes
// narrow the principal's roles to what the token endpoint granted.
func (c *tokenClaims) principal() *core.Principal {
	tp := c.Principal
	p := &core.Principal{
		ID: tp.ID, Name: tp.Name, Kind: tp.Kind, Source: tp.Source,
		Roles: tp.Roles, TokenID: tp.TokenID,
	}
	if p.Kind == "" {
		p.Kind = core.PrincipalAnonymous
	}
	p.Scopes = append([]core.Privilege{}, c.Privileges...)
	return p
}

// original is the principal as authenticated at the token endpoint, with the
// scopes of the credentials used there.
func (c *tokenClaims) original() *core.Principal {
	p := c.principal()
	p.Scopes = c.Principal.Scopes
	return p
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
