package federation

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Fetch and cache bounds for issuer discovery and key sets.
//
// fetchTimeout matches the default request timeout to sign-in providers
// (auth.oidc.timeout) and maxDocumentBytes the limit of the sign-in
// connection test (settings.getJSON). keysMaxAge, refreshMinInterval and
// maxKeys are local choices awaiting the resilience budget.
const (
	fetchTimeout     = 10 * time.Second
	maxDocumentBytes = 1 << 20
	// keysMaxAge: cached keys are fetched again after this long, so a key
	// the issuer withdrew stops being accepted.
	keysMaxAge = time.Hour
	// refreshMinInterval: an unknown key id (or a failed fetch) fetches the
	// keys again at most this often per issuer, so tokens with made-up key
	// ids cannot make Hangar hammer the issuer.
	refreshMinInterval = 30 * time.Second
	// maxKeys bounds the keys kept per issuer.
	maxKeys = 100
	// minRSABits: RSA keys shorter than this are ignored.
	minRSABits = 2048
)

// errUnknownKey: no key of the issuer verifies the token's key id and alg.
var errUnknownKey = errors.New("no published key matches the token's key id and algorithm")

// keyCache fetches and caches the signing keys of trusted issuers. Only
// issuers of enabled trust policies are looked up (Exchange checks that
// before asking), so a token cannot make Hangar fetch from an address of
// its choosing.
type keyCache struct {
	client    *http.Client
	allowHTTP bool
	now       func() time.Time

	mu      sync.Mutex
	issuers map[string]*issuerKeys
	fetches singleflight.Group
}

type issuerKeys struct {
	keys    []jose.JSONWebKey
	fetched time.Time // last successful fetch
	tried   time.Time // last attempt
	lastErr error     // error of the last attempt, if it failed
	// unknownAt is when a key id missing from fresh keys last caused a
	// fetch: rotation is picked up at once, made-up ids at most every
	// refreshMinInterval.
	unknownAt time.Time
}

func newKeyCache(client *http.Client, allowHTTP bool, now func() time.Time) *keyCache {
	return &keyCache{client: client, allowHTTP: allowHTTP, now: now, issuers: map[string]*issuerKeys{}}
}

// key returns the public key of issuer with key id kid usable with alg.
// Fresh cached keys are used as they are; stale keys fetch the key set
// again, and so does an unknown kid (at most every refreshMinInterval). A
// failed fetch is not retried within refreshMinInterval. Fetch failures
// wrap core.ErrUpstream.
func (c *keyCache) key(ctx context.Context, issuer, kid, alg string) (any, error) {
	if k, err, done := c.cached(issuer, kid, alg); done {
		return k, err
	}
	keys, err := c.refresh(ctx, issuer)
	if err != nil {
		return nil, err
	}
	if k := findKey(keys, kid, alg); k != nil {
		return k, nil
	}
	return nil, errUnknownKey
}

// cached answers from the cache when it can (done): a fresh key, a recent
// fetch failure, or an unknown kid seen too recently to fetch again.
// Otherwise the caller fetches.
func (c *keyCache) cached(issuer, kid, alg string) (key any, err error, done bool) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.issuers[issuer]
	if e == nil {
		return nil, nil, false
	}
	fresh := !e.fetched.IsZero() && now.Sub(e.fetched) < keysMaxAge
	if fresh {
		if k := findKey(e.keys, kid, alg); k != nil {
			return k, nil, true
		}
	}
	if e.lastErr != nil && now.Sub(e.tried) < refreshMinInterval {
		return nil, e.lastErr, true
	}
	if fresh {
		if now.Sub(e.unknownAt) < refreshMinInterval {
			return nil, errUnknownKey, true
		}
		e.unknownAt = now
	}
	return nil, nil, false
}

// refresh fetches issuer's keys once for all concurrent callers. The fetch
// runs on its own deadline (fetchTimeout), detached from the first caller,
// so one client hanging up does not fail the others; each caller stops
// waiting when its own context ends.
func (c *keyCache) refresh(ctx context.Context, issuer string) ([]jose.JSONWebKey, error) {
	ch := c.fetches.DoChan(issuer, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
		defer cancel()
		keys, err := c.fetch(fctx, issuer)
		now := c.now()
		c.mu.Lock()
		defer c.mu.Unlock()
		e := c.issuers[issuer]
		if e == nil {
			e = &issuerKeys{}
			c.issuers[issuer] = e
		}
		e.tried = now
		if err != nil {
			e.lastErr = fmt.Errorf("issuer %s: %w: %w", issuer, err, core.ErrUpstream)
			return nil, e.lastErr
		}
		e.keys, e.fetched, e.lastErr = keys, now, nil
		return keys, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		keys, _ := res.Val.([]jose.JSONWebKey)
		return keys, nil
	}
}

// discovery is the part of the OpenID Provider metadata this package reads.
type discovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

// fetch reads the discovery document of issuer and the key set it names.
func (c *keyCache) fetch(ctx context.Context, issuer string) ([]jose.JSONWebKey, error) {
	var doc discovery
	if err := c.getJSON(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &doc); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if doc.Issuer != issuer {
		return nil, fmt.Errorf("discovery: the provider calls itself %q", doc.Issuer)
	}
	if err := checkURL(doc.JWKSURI, c.allowHTTP); err != nil {
		return nil, fmt.Errorf("discovery: jwks_uri: %w", err)
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := c.getJSON(ctx, doc.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("key set: %w", err)
	}
	keys := make([]jose.JSONWebKey, 0, min(len(set.Keys), maxKeys))
	for _, raw := range set.Keys {
		var k jose.JSONWebKey
		// Keys of types this server does not use are skipped, not fatal.
		if err := k.UnmarshalJSON(raw); err != nil || !k.Valid() || !k.IsPublic() {
			continue
		}
		keys = append(keys, k)
		if len(keys) == maxKeys {
			break
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("key set: no usable public keys")
	}
	return keys, nil
}

func (c *keyCache) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDocumentBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes+1))
	if err != nil {
		return fmt.Errorf("GET %s: %w", u, err)
	}
	if len(b) > maxDocumentBytes {
		return fmt.Errorf("GET %s: larger than %d bytes", u, maxDocumentBytes)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("GET %s: not JSON: %w", u, err)
	}
	return nil
}

// findKey returns the public key of keys with id kid (any key when the
// token names none and exactly one fits) that may verify alg.
func findKey(keys []jose.JSONWebKey, kid, alg string) any {
	var found any
	n := 0
	for _, k := range keys {
		if kid != "" && k.KeyID != kid {
			continue
		}
		if (k.Use != "" && k.Use != "sig") || (k.Algorithm != "" && k.Algorithm != alg) || !keyFits(k.Key, alg) {
			continue
		}
		found = k.Key
		n++
	}
	if n != 1 {
		return nil
	}
	return found
}

// keyFits reports whether key can verify alg: an RSA key of at least
// minRSABits for RS256, a P-256 key for ES256. Checking the key type
// here, not only in the signature check, keeps a key published for one
// algorithm from being used with another.
func keyFits(key any, alg string) bool {
	switch k := key.(type) {
	case *rsa.PublicKey:
		return alg == string(jose.RS256) && k.N.BitLen() >= minRSABits
	case *ecdsa.PublicKey:
		return alg == string(jose.ES256) && k.Curve == elliptic.P256()
	}
	return false
}

// checkURL accepts absolute https URLs (http too when allowHTTP) without
// credentials, query or fragment.
func checkURL(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || raw == "":
		return errors.New("not a URL")
	case u.Scheme != "https" && (!allowHTTP || u.Scheme != "http"):
		return errors.New("must be an https URL")
	case u.Host == "" || u.Hostname() == "":
		return errors.New("has no host")
	case u.User != nil:
		return errors.New("must not contain credentials")
	case u.RawQuery != "" || u.Fragment != "" || u.ForceQuery:
		return errors.New("must not have a query or fragment")
	}
	return nil
}

// newHTTPClient returns the client for issuer requests: TLS 1.2 or newer,
// every dialled address checked by guard (the policy for services an
// administrator points Hangar at, proxy.Guard.CheckServiceAddr: internal
// issuers are fine, cloud metadata endpoints are not), redirects only to
// https (unless allowHTTP), and fetchTimeout per request.
func newHTTPClient(guard func(netip.Addr) error, allowHTTP bool) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	d := &net.Dialer{}
	if guard != nil {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			a, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("dial %s: not an IP address", address)
			}
			return guard(a.WithZone(""))
		}
	}
	tr.DialContext = d.DialContext
	return &http.Client{
		Transport: tr,
		Timeout:   fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 10 is net/http's own limit, which a CheckRedirect replaces.
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if req.URL.Scheme != "https" && (!allowHTTP || req.URL.Scheme != "http") {
				return fmt.Errorf("redirect to %s refused: must be https", req.URL.Redacted())
			}
			return nil
		},
	}
}
