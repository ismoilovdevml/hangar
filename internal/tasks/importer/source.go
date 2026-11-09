package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/version"
)

// errNotFound is a 404 (or 410) of the source.
var errNotFound = errors.New("not found at the source")

// errNoListing: the source cannot list its packages; names must be given.
var errNoListing = errors.New("the source does not list its packages")

// Bounds of what is read into memory from a source.
const (
	maxDocBytes   = 64 << 20 // metadata documents (packuments, indexes)
	maxRetries    = 4
	maxRetryAfter = 60 * time.Second
)

// limiter spaces requests to at most rps per second.
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newLimiter(rps float64) *limiter {
	return &limiter{interval: time.Duration(float64(time.Second) / rps)}
}

func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	at := l.next
	if at.Before(now) {
		at = now
	}
	l.next = at.Add(l.interval)
	l.mu.Unlock()
	if d := time.Until(at); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-t.C:
		}
	}
	return nil
}

// source is the HTTP client of one source registry.
type source struct {
	base   *url.URL // with a trailing slash
	client *http.Client
	creds  Source
	limit  *limiter
	agent  string

	mu     sync.Mutex
	tokens map[string]string // OCI bearer tokens by scope
	// requests counts requests sent (tests, progress).
	requests int64
}

func (s *Service) newSource(src Source, rps float64) (*source, error) {
	open, err := s.openSource(src)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimPrefix(src.URL, "sparse+"))
	if err != nil {
		return nil, fmt.Errorf("source.url: %w", core.ErrInvalid)
	}
	if !strings.HasSuffix(u.Path, "/") && !strings.HasSuffix(u.Path, ".json") {
		u.Path += "/"
	}
	c, err := s.opts.HTTPClient(u.String())
	if err != nil {
		return nil, fmt.Errorf("source client: %w", err)
	}
	return &source{base: u, client: c, creds: open, limit: newLimiter(rps), agent: version.UserAgent(), tokens: map[string]string{}}, nil
}

// resolve turns a reference (relative to the source URL, or absolute) into a URL.
func (s *source) resolve(ref string) (*url.URL, error) {
	r, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("source reference %q: %w", ref, core.ErrInvalid)
	}
	u := s.base.ResolveReference(r)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("source reference %q: not http(s): %w", ref, core.ErrInvalid)
	}
	return u, nil
}

func (s *source) sameHost(u *url.URL) bool {
	return strings.EqualFold(u.Scheme, s.base.Scheme) && strings.EqualFold(u.Host, s.base.Host)
}

func (s *source) Requests() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// do sends a GET (or HEAD) for ref with the source's credentials (own host
// only), retrying transport errors, 429 and 5xx with backoff and answering
// OCI bearer challenges. Non-2xx answers are returned as responses; the
// caller closes the body.
func (s *source) do(ctx context.Context, method, ref string, hdr http.Header) (*http.Response, error) {
	u, err := s.resolve(ref)
	if err != nil {
		return nil, err
	}
	var lastErr error
	challenged := false
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := s.limit.wait(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		req.Header.Set("User-Agent", s.agent)
		scope := hdr.Get(ociScopeHeader)
		req.Header.Del(ociScopeHeader)
		s.authorize(req, u, scope)
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()
		resp, err := s.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			lastErr = fmt.Errorf("%s %s: %w", method, core.RedactURL(u.String()), err)
			if !sleepCtx(ctx, backoff(attempt, nil)) {
				return nil, context.Cause(ctx)
			}
			continue
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && !challenged:
			challenged = true
			if ok, err := s.answerChallenge(ctx, resp, scope); err != nil {
				drain(resp)
				return nil, err
			} else if ok {
				drain(resp)
				attempt--
				continue
			}
			return resp, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusBadGateway ||
			resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout:
			if attempt == maxRetries {
				return resp, nil
			}
			wait := backoff(attempt, resp)
			drain(resp)
			lastErr = fmt.Errorf("%s %s: %s", method, core.RedactURL(u.String()), resp.Status)
			if !sleepCtx(ctx, wait) {
				return nil, context.Cause(ctx)
			}
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

func backoff(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if n, err := strconv.Atoi(ra); err == nil && n >= 0 {
				return min(time.Duration(n)*time.Second, maxRetryAfter)
			}
			if t, err := http.ParseTime(ra); err == nil {
				return min(max(time.Until(t), 0), maxRetryAfter)
			}
		}
	}
	return time.Duration(250<<attempt) * time.Millisecond
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// authorize sets the credentials for a request to u: a bearer token obtained
// from an OCI challenge for scope, else the configured credentials, which are
// only ever sent to the source's own host.
func (s *source) authorize(req *http.Request, u *url.URL, scope string) {
	if !s.sameHost(u) {
		return
	}
	s.mu.Lock()
	tok := s.tokens[scope]
	if tok == "" {
		tok = s.tokens[""]
	}
	s.mu.Unlock()
	switch {
	case tok != "":
		req.Header.Set("Authorization", "Bearer "+tok)
	case s.creds.Token != "":
		req.Header.Set("Authorization", "Bearer "+s.creds.Token)
	case s.creds.Username != "":
		req.SetBasicAuth(s.creds.Username, s.creds.Password)
	}
}

// ociScopeHeader carries the token scope of an OCI request to do (never sent).
const ociScopeHeader = "X-Hangar-Import-Scope"

// answerChallenge handles "WWW-Authenticate: Bearer realm=...": it asks the
// realm for a token (with the source's credentials) and reports whether the
// request should be repeated.
func (s *source) answerChallenge(ctx context.Context, resp *http.Response, scope string) (bool, error) {
	ch := parseChallenge(resp.Header.Get("WWW-Authenticate"))
	if ch == nil || ch["realm"] == "" {
		return false, nil
	}
	realm, err := url.Parse(ch["realm"])
	if err != nil || (realm.Scheme != "https" && realm.Scheme != "http") {
		return false, nil
	}
	q := realm.Query()
	if svc := ch["service"]; svc != "" {
		q.Set("service", svc)
	}
	sc := ch["scope"]
	if sc == "" {
		sc = scope
	}
	if sc != "" {
		q.Set("scope", sc)
	}
	realm.RawQuery = q.Encode()
	if err := s.limit.wait(ctx); err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return false, nil //nolint:nilerr // an unusable challenge: the 401 stands
	}
	req.Header.Set("User-Agent", s.agent)
	// The token service of a registry is often another host: it gets the
	// registry credentials as the protocol requires.
	switch {
	case s.creds.Username != "":
		req.SetBasicAuth(s.creds.Username, s.creds.Password)
	case s.creds.Token != "":
		req.Header.Set("Authorization", "Bearer "+s.creds.Token)
	}
	tr, err := s.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("token service %s: %w", core.RedactURL(realm.Host), err)
	}
	defer drain(tr)
	if tr.StatusCode != http.StatusOK {
		return false, fmt.Errorf("token service %s answered %s: %w", realm.Host, tr.Status, core.ErrUnauthenticated)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(tr.Body, 1<<20)).Decode(&body); err != nil {
		return false, fmt.Errorf("token service %s: %w", realm.Host, err)
	}
	tok := body.Token
	if tok == "" {
		tok = body.AccessToken
	}
	if tok == "" {
		return false, nil
	}
	s.mu.Lock()
	s.tokens[scope] = tok
	s.mu.Unlock()
	return true, nil
}

// parseChallenge parses `Bearer realm="...",service="...",scope="..."`.
func parseChallenge(h string) map[string]string {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return nil
	}
	out := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			end := strings.IndexByte(v[1:], '"')
			if end < 0 {
				out[k] = v[1:]
				break
			}
			out[k], rest = v[1:end+1], v[end+2:]
		} else {
			val, after, _ := strings.Cut(v, ",")
			out[k], rest = strings.TrimSpace(val), after
		}
	}
	return out
}

// statusErr turns a non-2xx response into an error (and closes it).
func statusErr(resp *http.Response, what string) error {
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%s: %w", what, errNotFound)
	case http.StatusUnauthorized:
		return fmt.Errorf("%s: the source asks for credentials (%s): %w", what, resp.Status, core.ErrUnauthenticated)
	case http.StatusForbidden:
		return fmt.Errorf("%s: the source refused access (%s): %w", what, resp.Status, core.ErrForbidden)
	}
	return fmt.Errorf("%s: the source answered %s: %w", what, resp.Status, core.ErrUpstream)
}

// open GETs ref and returns the body of a 2xx answer.
func (s *source) open(ctx context.Context, ref string, hdr http.Header) (*http.Response, error) {
	resp, err := s.do(ctx, http.MethodGet, ref, hdr)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, statusErr(resp, "GET "+ref)
	}
	return resp, nil
}

// bytes GETs ref into memory (at most maxDocBytes).
func (s *source) bytes(ctx context.Context, ref string, hdr http.Header) ([]byte, http.Header, error) {
	resp, err := s.open(ctx, ref, hdr)
	if err != nil {
		return nil, nil, err
	}
	defer drain(resp)
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxDocBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: %w", ref, err)
	}
	if len(b) > maxDocBytes {
		return nil, nil, fmt.Errorf("GET %s: document larger than %d bytes: %w", ref, maxDocBytes, core.ErrInvalid)
	}
	return b, resp.Header, nil
}

// json GETs ref and decodes it into v.
func (s *source) json(ctx context.Context, ref string, v any) error {
	b, _, err := s.bytes(ctx, ref, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("GET %s: not the expected JSON: %w", ref, core.ErrUpstream)
	}
	return nil
}

func lastModified(h http.Header) time.Time {
	t, err := http.ParseTime(h.Get("Last-Modified"))
	if err != nil {
		return time.Time{}
	}
	return t
}
