package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// upstream implements core.Upstream for one proxy repository.
type upstream struct {
	name        string
	remoteRaw   string
	base        *url.URL
	fingerprint string

	client       *http.Client // shared transport, guarded redirects
	guard        *Guard
	proxied      func(*url.URL) bool
	maxRedirects int

	breaker *gobreaker.TwoStepCircuitBreaker[struct{}]
	flights flightGroup
	neg     negCache
	negTTL  time.Duration
	tokens  tokenStore
	tf      *tokenFetcher
	stats   stats

	retries int
	backoff time.Duration
	slow    time.Duration
	now     func() time.Time
	log     *slog.Logger
	onState func(u *upstream, prev, cur State)
}

var _ core.Upstream = (*upstream)(nil)

// errServerStatus marks a 5xx answer for the circuit breaker.
var errServerStatus = errors.New("remote answered 5xx")

// Fetch implements core.Upstream.
//
// Order of operations: path validation, negative cache (a remembered 404 is
// answered without a request), then one shared request per identical
// (method, URL, headers, validators) among concurrent callers. That request
// passes the circuit breaker, the SSRF guard (remote and every redirect hop),
// retries GET/HEAD on connection errors and 5xx with jittered exponential
// backoff, and answers an OCI token challenge once.
//
// Responses with any status are returned as responses, including 5xx after
// the last retry; transport failures, blocked addresses and an open circuit
// return errors wrapping core.ErrUpstream.
func (u *upstream) Fetch(ctx context.Context, req core.UpstreamRequest) (*core.UpstreamResponse, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodHead {
		return nil, fmt.Errorf("upstream method %q: want GET or HEAD: %w", method, core.ErrInvalid)
	}
	target, err := u.resolve(req.Path)
	if err != nil {
		return nil, err
	}
	if u.negTTL > 0 && u.neg.hit(target.String(), u.now()) {
		return &core.UpstreamResponse{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: http.NoBody}, nil
	}
	return u.flights.do(ctx, flightKey(method, target, req), func(ctx context.Context) (*core.UpstreamResponse, error) {
		return u.fetch(ctx, method, target, req)
	})
}

// resolve joins path to the remote URL, keeping its escaping (npm's
// "@scope%2fname") and rejecting dot segments that would climb out of it.
func (u *upstream) resolve(path string) (*url.URL, error) {
	p := strings.TrimPrefix(path, "/")
	pathOnly := p
	if i := strings.IndexAny(pathOnly, "?#"); i >= 0 {
		pathOnly = pathOnly[:i]
	}
	for seg := range strings.SplitSeq(pathOnly, "/") {
		if s, err := url.PathUnescape(seg); err != nil || s == ".." || s == "." {
			return nil, fmt.Errorf("upstream path %q: %w", path, core.ErrInvalid)
		}
	}
	t, err := url.Parse(strings.TrimSuffix(u.remoteRaw, "/") + "/" + p)
	if err != nil || t.Host != u.base.Host || t.Scheme != u.base.Scheme {
		return nil, fmt.Errorf("upstream path %q: %w", path, core.ErrInvalid)
	}
	t.Fragment = ""
	return t, nil
}

func flightKey(method string, target *url.URL, req core.UpstreamRequest) string {
	var b strings.Builder
	b.WriteString(method)
	b.WriteByte(0)
	b.WriteString(target.String())
	b.WriteByte(0)
	b.WriteString(req.Conditional.ETag)
	b.WriteByte(0)
	if !req.Conditional.LastModified.IsZero() {
		b.WriteString(req.Conditional.LastModified.UTC().Format(http.TimeFormat))
	}
	b.WriteByte(0)
	_ = req.Header.Write(&b) // sorted by key
	return b.String()
}

func (u *upstream) fetch(ctx context.Context, method string, target *url.URL, req core.UpstreamRequest) (*core.UpstreamResponse, error) {
	done, err := u.breaker.Allow()
	if err != nil {
		u.noteState()
		return nil, fmt.Errorf("upstream %s: %w: %w", u.name, err, core.ErrUpstream)
	}
	start := u.now()
	resp, err := u.roundTrip(ctx, method, target, req) //nolint:bodyclose // the body is handed to the caller
	if err != nil {
		done(err)
		if errors.Is(err, ErrBlocked) {
			u.stats.setBlocked(true)
		}
		u.noteState()
		u.log.Warn("upstream request failed", "repository", u.name, "url", target.Redacted(), "error", err)
		return nil, fmt.Errorf("upstream %s: %w: %w", u.name, err, core.ErrUpstream)
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		done(errServerStatus)
	} else {
		done(nil)
	}
	now := u.now()
	u.stats.observe(now.Sub(start), now)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		u.stats.setBlocked(true)
	case resp.StatusCode < http.StatusInternalServerError:
		u.stats.setBlocked(false)
	}
	if resp.StatusCode == http.StatusNotFound && u.negTTL > 0 {
		u.neg.add(target.String(), now, u.negTTL)
	}
	u.noteState()
	return toUpstreamResponse(method, resp), nil
}

func toUpstreamResponse(method string, resp *http.Response) *core.UpstreamResponse {
	out := &core.UpstreamResponse{
		StatusCode:    resp.StatusCode,
		Header:        resp.Header,
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		NotModified:   resp.StatusCode == http.StatusNotModified,
		ETag:          resp.Header.Get("ETag"),
	}
	if lm, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		out.LastModified = lm
	}
	if method == http.MethodHead || out.NotModified || resp.ContentLength == 0 {
		discard(resp)
		out.Body = http.NoBody
	}
	return out
}

// roundTrip runs the request with retries.
func (u *upstream) roundTrip(ctx context.Context, method string, target *url.URL, req core.UpstreamRequest) (*http.Response, error) {
	if err := u.check(ctx, target); err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		resp, err := u.attempt(ctx, method, target, req)
		if attempt >= u.retries || !retryable(resp, err) {
			return resp, err
		}
		if resp != nil {
			discard(resp)
		}
		if err := sleep(ctx, u.backoffFor(attempt)); err != nil {
			return nil, err
		}
	}
}

func retryable(resp *http.Response, err error) bool {
	if err != nil {
		return !errors.Is(err, ErrBlocked) && !errors.Is(err, context.Canceled)
	}
	switch resp.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoffFor returns base * 2^attempt, jittered to between 50% and 150%.
func (u *upstream) backoffFor(attempt int) time.Duration {
	d := u.backoff << min(attempt, 6)
	return d/2 + rand.N(d+1) //nolint:gosec // jitter, not a secret
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// attempt sends one request, answering a Bearer token challenge once.
func (u *upstream) attempt(ctx context.Context, method string, target *url.URL, req core.UpstreamRequest) (*http.Response, error) {
	hreq, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hreq.Header.Add(k, v)
		}
	}
	if req.Conditional.ETag != "" {
		hreq.Header.Set("If-None-Match", req.Conditional.ETag)
	}
	if !req.Conditional.LastModified.IsZero() {
		hreq.Header.Set("If-Modified-Since", req.Conditional.LastModified.UTC().Format(http.TimeFormat))
	}
	// An Authorization header set by the caller is theirs to manage.
	callerAuth := hreq.Header.Get("Authorization") != ""
	scope := scopeKey(req.Path)
	used, usedOK := bearerChallenge{}, false
	if !callerAuth {
		var tok string
		if used, tok, usedOK = u.tokens.cached(scope, u.now()); usedOK {
			hreq.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := u.client.Do(hreq)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || callerAuth {
		return resp, err
	}
	// A Basic challenge needs no dance: configured basic credentials were
	// already sent by the client; the 401 means they are missing or wrong.
	ch, ok := findBearer(resp.Header)
	if !ok {
		return resp, nil
	}
	if usedOK {
		u.tokens.drop(used)
	}
	tok, exp, err := u.tokens.get(ctx, u.tf, ch)
	if err != nil {
		if errors.Is(err, ErrBlocked) {
			discard(resp)
			return nil, err
		}
		u.log.Warn("upstream token request failed", "repository", u.name, "realm", ch.Realm, "error", err)
		return resp, nil // the 401 stands
	}
	discard(resp)
	u.tokens.remember(scope, ch, tok, exp)
	retry := hreq.Clone(ctx)
	retry.Header.Set("Authorization", "Bearer "+tok)
	resp, err = u.client.Do(retry)
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		u.tokens.drop(ch)
	}
	return resp, err
}

// check applies the SSRF guard to a URL about to be requested.
func (u *upstream) check(ctx context.Context, t *url.URL) error {
	return u.guard.Check(ctx, t, u.proxied(t))
}

func (u *upstream) checkRedirect(r *http.Request, via []*http.Request) error {
	if len(via) > u.maxRedirects {
		return fmt.Errorf("stopped after %d redirects", u.maxRedirects)
	}
	return u.check(r.Context(), r.URL)
}

func (u *upstream) noteState() {
	if prev, cur, changed := u.changedState(); changed && u.onState != nil {
		u.onState(u, prev, cur)
	}
}

// discard drains a little of an unwanted body, so the connection can be
// reused, and closes it.
func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
