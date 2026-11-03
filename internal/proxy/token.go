package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// challenge is one parsed WWW-Authenticate challenge (RFC 9110 section 11.6.1).
type challenge struct {
	Scheme string // lower-case: "bearer", "basic"
	Params map[string]string
}

// parseChallenges parses WWW-Authenticate header values. One value may hold
// several challenges: `Basic realm="x", Bearer realm="y",service="z"`.
func parseChallenges(values []string) []challenge {
	var out []challenge
	for _, v := range values {
		p := &authParser{s: v}
		var cur *challenge
		for {
			p.skip(" \t,")
			if p.eof() {
				break
			}
			tok := p.token()
			if tok == "" { // garbage: give up on this value
				break
			}
			p.skip(" \t")
			if p.peek() == '=' && cur != nil {
				p.i++
				p.skip(" \t")
				cur.Params[strings.ToLower(tok)] = p.value()
				continue
			}
			out = append(out, challenge{Scheme: strings.ToLower(tok), Params: map[string]string{}})
			cur = &out[len(out)-1]
		}
	}
	return out
}

type authParser struct {
	s string
	i int
}

func (p *authParser) eof() bool { return p.i >= len(p.s) }

func (p *authParser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.s[p.i]
}

func (p *authParser) skip(chars string) {
	for !p.eof() && strings.IndexByte(chars, p.s[p.i]) >= 0 {
		p.i++
	}
}

func (p *authParser) token() string {
	start := p.i
	for !p.eof() && strings.IndexByte(" \t,=\"", p.s[p.i]) < 0 {
		p.i++
	}
	return p.s[start:p.i]
}

func (p *authParser) value() string {
	if p.peek() != '"' {
		return p.token()
	}
	p.i++
	var b strings.Builder
	for !p.eof() {
		c := p.s[p.i]
		p.i++
		switch c {
		case '\\':
			if !p.eof() {
				b.WriteByte(p.s[p.i])
				p.i++
			}
		case '"':
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// bearerChallenge is a token-service challenge as used by OCI registries:
// `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`.
type bearerChallenge struct {
	Realm   string
	Service string
	Scope   string // space-separated scopes
}

func (c bearerChallenge) key() string { return c.Realm + "\x00" + c.Service + "\x00" + c.Scope }

// findBearer returns the first Bearer challenge with a realm.
func findBearer(h http.Header) (bearerChallenge, bool) {
	for _, c := range parseChallenges(h.Values("WWW-Authenticate")) {
		if c.Scheme == "bearer" && c.Params["realm"] != "" {
			return bearerChallenge{Realm: c.Params["realm"], Service: c.Params["service"], Scope: c.Params["scope"]}, true
		}
	}
	return bearerChallenge{}, false
}

// scopeKey groups request paths that share a token scope: the OCI repository
// ("v2/library/nginx") for registry API paths, otherwise the whole remote.
func scopeKey(path string) string {
	p := strings.TrimPrefix(path, "/")
	if !strings.HasPrefix(p, "v2/") {
		return ""
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	for _, marker := range []string{"/manifests/", "/blobs/", "/tags/", "/referrers/"} {
		if i := strings.LastIndex(p, marker); i > len("v2") {
			return p[:i]
		}
	}
	return p
}

// maxScopeHints bounds the remembered path -> challenge mappings.
const maxScopeHints = 10000

// tokenStore caches registry tokens by challenge (realm, service, scope)
// until shortly before they expire, and remembers which challenge a path
// scope produced so later requests send the token up front instead of
// paying for a 401 round trip.
type tokenStore struct {
	mu     sync.Mutex
	tokens map[string]cachedToken // challenge key -> token
	hints  map[string]bearerChallenge
	group  singleflight.Group
}

type cachedToken struct {
	token   string
	expires time.Time
}

// cached returns a valid token for the path scope, if one is known.
func (s *tokenStore) cached(scope string, now time.Time) (bearerChallenge, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.hints[scope]
	if !ok {
		return bearerChallenge{}, "", false
	}
	t, ok := s.tokens[ch.key()]
	if !ok || !now.Before(t.expires) {
		delete(s.tokens, ch.key())
		return bearerChallenge{}, "", false
	}
	return ch, t.token, true
}

func (s *tokenStore) remember(scope string, ch bearerChallenge, tok string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[string]cachedToken{}
		s.hints = map[string]bearerChallenge{}
	}
	if len(s.hints) >= maxScopeHints {
		clear(s.hints)
	}
	if len(s.tokens) >= maxScopeHints {
		clear(s.tokens)
	}
	s.hints[scope] = ch
	s.tokens[ch.key()] = cachedToken{token: tok, expires: expires}
}

func (s *tokenStore) drop(ch bearerChallenge) {
	s.mu.Lock()
	delete(s.tokens, ch.key())
	s.mu.Unlock()
}

// tokenFetcher obtains tokens from a challenge's realm.
type tokenFetcher struct {
	client *http.Client
	remote *url.URL
	auth   *core.UpstreamAuth
	check  func(context.Context, *url.URL) error // SSRF guard
	now    func() time.Time
}

type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

// get returns a token for ch, sharing one realm request among concurrent callers.
func (s *tokenStore) get(ctx context.Context, f *tokenFetcher, ch bearerChallenge) (string, time.Time, error) {
	type result struct {
		token   string
		expires time.Time
	}
	v, err, _ := s.group.Do(ch.key(), func() (any, error) {
		tok, exp, err := f.fetch(ctx, ch)
		return result{tok, exp}, err
	})
	if err != nil {
		return "", time.Time{}, err
	}
	r := v.(result)
	return r.token, r.expires, nil
}

func (f *tokenFetcher) fetch(ctx context.Context, ch bearerChallenge) (string, time.Time, error) {
	realm, err := url.Parse(ch.Realm)
	if err != nil || realm.Host == "" {
		return "", time.Time{}, fmt.Errorf("token realm %q: invalid URL", ch.Realm)
	}
	if err := f.check(ctx, realm); err != nil {
		return "", time.Time{}, fmt.Errorf("token realm %s: %w", realm.Redacted(), err)
	}
	q := realm.Query()
	if ch.Service != "" {
		q.Set("service", ch.Service)
	}
	for sc := range strings.FieldsSeq(ch.Scope) {
		q.Add("scope", sc)
	}
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token request: %w", err)
	}
	// Credentials go to the token service only over TLS, or when the realm
	// is the remote's own origin.
	sameOrigin := realm.Scheme == f.remote.Scheme && strings.EqualFold(realm.Host, f.remote.Host)
	if f.auth != nil && (realm.Scheme == "https" || sameOrigin) {
		switch f.auth.Kind {
		case core.UpstreamAuthBasic:
			req.SetBasicAuth(f.auth.Username, f.auth.Password)
		case core.UpstreamAuthBearer:
			req.Header.Set("Authorization", "Bearer "+f.auth.Token)
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token request to %s: %w", realm.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return "", time.Time{}, fmt.Errorf("token service %s answered %d: %w", realm.Host, resp.StatusCode, errTokenDenied)
	}
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return "", time.Time{}, fmt.Errorf("token response from %s: %w", realm.Host, err)
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return "", time.Time{}, fmt.Errorf("token response from %s: no token", realm.Host)
	}
	life := time.Duration(tr.ExpiresIn) * time.Second
	if life <= 0 {
		life = 60 * time.Second // registry token spec default
	}
	margin := min(max(life/10, time.Second), 30*time.Second)
	return tok, f.now().Add(life - margin), nil
}

// errTokenDenied marks a token service refusing our credentials.
var errTokenDenied = errors.New("token denied")
