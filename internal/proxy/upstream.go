package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	related      []string // ProxyConfig.RelatedHosts
	offline      bool

	breaker  *gobreaker.TwoStepCircuitBreaker[struct{}] // the remote's own host
	breakers breakerSet                                 // related hosts
	throttle throttleGate
	slots    chan struct{} // nil = unlimited concurrent requests
	flights  flightGroup
	neg      negCache
	negTTL   time.Duration
	tokens   tokenStore
	tf       *tokenFetcher
	stats    stats
	dns      dnsFailures

	retries        int
	backoff        time.Duration
	retryAfterMax  time.Duration // total Retry-After wait per request
	throttleMax    time.Duration // longest a 429 keeps a host gated
	bodyTimeout    time.Duration
	maxResumes     int
	slow           time.Duration
	now            func() time.Time
	log            *slog.Logger
	onState        func(u *upstream, prev, cur State)
	newBreakerFunc func(name string) *gobreaker.TwoStepCircuitBreaker[struct{}]
}

var _ core.Upstream = (*upstream)(nil)

// Errors the circuit breaker sees. errThrottled is excluded from its counts:
// a remote that rate-limits us is up, and tripping the breaker would turn a
// short wait into CircuitOpenFor of failed requests.
var (
	errServerStatus = errors.New("remote answered 5xx")
	errThrottled    = errors.New("remote answered 429")
	// errDNSBurst stands for a transient DNS failure that is not counted
	// because one was counted less than dnsFailureSpacing ago.
	errDNSBurst = errors.New("name resolution failed again")
)

// dnsFailureSpacing: transient DNS failures (timeouts, SERVFAIL) count as
// at most one breaker failure per spacing. A burst of requests failing on
// the same unanswered lookup is one failure, not one per request; a DNS
// outage that lasts trips the breaker after the usual number of failures.
// "No such host" counts like any other connection error.
const dnsFailureSpacing = time.Second

// dnsFailures rate-limits the breaker failures of transient DNS errors.
type dnsFailures struct {
	mu      sync.Mutex
	counted time.Time
}

// count reports whether a transient DNS failure at now counts.
func (d *dnsFailures) count(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.counted.IsZero() && now.Sub(d.counted) < dnsFailureSpacing {
		return false
	}
	d.counted = now
	return true
}

// transientDNS reports a name-resolution failure that is not a definite
// "no such host": a timeout, a temporary failure or a misbehaving server.
func transientDNS(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && !de.IsNotFound
}

// ErrOfflineMode is wrapped by Fetch errors of a repository in offline mode
// (ProxyConfig.Offline), together with core.ErrNotFound and core.ErrUpstream.
var ErrOfflineMode = errors.New("proxy repository is in offline mode")

// Fetch implements core.Upstream.
//
// Order of operations: target validation (Path under the remote, or URL on
// the remote's or a related host), offline mode, negative cache (a
// remembered 404 is answered without a request), the rate-limit gate (a host
// that answered 429 is not asked again until its Retry-After passed), then
// one shared request per identical (method, URL, headers, validators) among
// concurrent callers. That request waits for a concurrency slot, passes the
// host's circuit breaker and the SSRF guard (target and every redirect hop),
// retries GET/HEAD on connection errors and 5xx with jittered exponential
// backoff and on 429 within the Retry-After budget, and answers an OCI token
// challenge once (the remote's own host only). Its body is guarded against
// stalls and resumed with a Range request when the remote supports it.
//
// Responses with any status but 429 are returned as responses, including
// 5xx after the last retry; transport failures, blocked addresses, an open
// circuit and offline mode return errors wrapping core.ErrUpstream, a final
// 429 an error wrapping core.ErrRateLimited (see core.Upstream).
func (u *upstream) Fetch(ctx context.Context, req core.UpstreamRequest) (*core.UpstreamResponse, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	post := method == http.MethodPost
	if method != http.MethodGet && method != http.MethodHead && !post {
		return nil, fmt.Errorf("upstream method %q: want GET, HEAD or POST: %w", method, core.ErrInvalid)
	}
	if req.Body != nil && !post {
		return nil, fmt.Errorf("upstream %s request with a body: %w", method, core.ErrInvalid)
	}
	target, primary, err := u.target(req)
	if err != nil {
		return nil, err
	}
	if u.offline {
		requestsTotal.WithLabelValues(u.name, target.Host, resultOffline).Inc()
		return nil, fmt.Errorf("upstream %s: %w, %s is not cached: %w: %w", u.name, ErrOfflineMode, target.Redacted(), core.ErrNotFound, core.ErrUpstream)
	}
	if !post && u.negTTL > 0 && u.neg.hit(target.String(), u.now()) {
		return &core.UpstreamResponse{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: http.NoBody}, nil
	}
	if wait, gated := u.throttle.gated(target.Host, u.now()); gated {
		requestsTotal.WithLabelValues(u.name, target.Host, resultRateLimited).Inc()
		return nil, rateLimited(u.name, target.Host, wait)
	}
	if post {
		// A query body is the caller's own: never shared with other callers.
		return u.fetch(ctx, method, target, req, primary)
	}
	return u.flights.do(ctx, flightKey(method, target, req), func(ctx context.Context) (*core.UpstreamResponse, error) {
		return u.fetch(ctx, method, target, req, primary)
	})
}

// target returns the URL to request and whether it is on the remote's own
// origin (which gets the remote's credentials and token flow).
func (u *upstream) target(req core.UpstreamRequest) (*url.URL, bool, error) {
	if req.URL == "" {
		t, err := u.resolve(req.Path)
		return t, true, err
	}
	t, err := url.Parse(req.URL)
	if err != nil || (t.Scheme != "http" && t.Scheme != "https") || t.Host == "" || t.User != nil || t.Opaque != "" {
		return nil, false, fmt.Errorf("upstream url %q: want an absolute http(s) URL: %w", core.RedactURL(req.URL), core.ErrInvalid)
	}
	t.Fragment = ""
	if t.Scheme == u.base.Scheme && strings.EqualFold(t.Host, u.base.Host) {
		return t, true, nil
	}
	if core.MatchHost(u.related, t.Host) || core.MatchHost(req.RelatedHosts, t.Host) {
		return t, false, nil
	}
	return nil, false, fmt.Errorf("upstream url %s: host %s is neither the remote nor a related host of repository %s: %w",
		t.Redacted(), t.Host, u.name, core.ErrInvalid)
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

func (u *upstream) breakerFor(host string, primary bool) *gobreaker.TwoStepCircuitBreaker[struct{}] {
	if primary {
		return u.breaker
	}
	return u.breakers.get(strings.ToLower(host), u.newBreakerFunc)
}

func (u *upstream) fetch(ctx context.Context, method string, target *url.URL, req core.UpstreamRequest, primary bool) (*core.UpstreamResponse, error) {
	host := target.Host
	release, err := u.acquire(ctx)
	if err != nil {
		return nil, err
	}
	br := u.breakerFor(host, primary)
	done, err := br.Allow()
	if err != nil {
		release()
		u.noteState()
		requestsTotal.WithLabelValues(u.name, host, resultCircuitOpen).Inc()
		return nil, fmt.Errorf("upstream %s (%s): %w: %w", u.name, host, err, core.ErrUpstream)
	}
	start := u.now()
	resp, cancel, err := u.roundTrip(ctx, method, target, req, primary)
	if err != nil {
		release()
		var tr *throttledError
		switch {
		case errors.As(err, &tr):
			done(errThrottled)
			u.throttle.hold(host, u.now(), min(max(tr.after, time.Second), u.throttleMax))
			requestsTotal.WithLabelValues(u.name, host, resultRateLimited).Inc()
			u.noteState()
			u.log.Warn("upstream rate limited", "repository", u.name, "url", target.Redacted(), "retry_after", tr.after)
			return nil, rateLimited(u.name, host, tr.after)
		case errors.Is(err, ErrBlocked):
			done(err)
			u.stats.setBlocked(ReasonAddressBlocked)
		case transientDNS(err):
			upstreamDNSFailures.WithLabelValues(u.name).Inc()
			if u.dns.count(u.now()) {
				done(err)
			} else {
				done(errDNSBurst)
			}
		default:
			done(err)
		}
		requestsTotal.WithLabelValues(u.name, host, resultError).Inc()
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
	requestDuration.WithLabelValues(u.name).Observe(now.Sub(start).Seconds())
	requestsTotal.WithLabelValues(u.name, host, statusClass(resp.StatusCode)).Inc()
	if primary {
		u.stats.observe(now.Sub(start), now)
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			u.stats.setBlocked(ReasonAuthFailed)
		case resp.StatusCode == http.StatusForbidden:
			u.stats.setBlocked(ReasonAccessDenied)
		case resp.StatusCode < http.StatusInternalServerError:
			u.stats.setBlocked("")
		}
	}
	if resp.StatusCode == http.StatusNotFound && u.negTTL > 0 && method != http.MethodPost {
		u.neg.add(target.String(), now, u.negTTL)
	}
	u.noteState()
	out := toUpstreamResponse(method, resp)
	if out.Body == http.NoBody {
		cancel()
		release()
		return out, nil
	}
	out.Body = u.newBody(ctx, bodyRequest{method: method, target: target, req: req, primary: primary}, resp, cancel, release)
	return out, nil
}

// acquire waits for a concurrency slot (ProxyConfig.MaxConcurrentRequests)
// and returns its release function.
func (u *upstream) acquire(ctx context.Context) (func(), error) {
	inflight.WithLabelValues(u.name).Inc()
	dec := func() { inflight.WithLabelValues(u.name).Dec() }
	if u.slots == nil {
		return sync.OnceFunc(dec), nil
	}
	select {
	case u.slots <- struct{}{}:
		return sync.OnceFunc(func() { <-u.slots; dec() }), nil
	case <-ctx.Done():
		dec()
		return nil, ctx.Err()
	}
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

// throttledError is a final 429 inside roundTrip.
type throttledError struct{ after time.Duration }

func (e *throttledError) Error() string { return "remote answered 429 Too Many Requests" }

// rateLimited is the error Fetch returns for a rate-limited host.
func rateLimited(repo, host string, after time.Duration) error {
	return &core.RetryAfterError{After: after, Err: fmt.Errorf("upstream %s: %s answered 429 Too Many Requests: %w: %w",
		repo, host, core.ErrRateLimited, core.ErrUpstream)}
}

// roundTrip runs the request with retries. It returns the final response
// with the cancel function of its request context, which the caller must
// call once the body is done.
func (u *upstream) roundTrip(ctx context.Context, method string, target *url.URL, req core.UpstreamRequest, primary bool) (*http.Response, context.CancelFunc, error) {
	if err := u.check(ctx, target); err != nil {
		return nil, nil, err
	}
	var waited time.Duration // Retry-After waits so far
	for attempt := 0; ; attempt++ {
		actx, cancel := context.WithCancel(ctx)
		resp, err := u.attempt(actx, method, target, req, primary)
		var wait time.Duration
		switch {
		case err == nil && resp.StatusCode == http.StatusTooManyRequests:
			after, ok := retryAfter(resp.Header, u.now())
			discard(resp)
			cancel()
			if !ok {
				after = u.backoffFor(attempt)
			}
			if attempt >= u.retries || waited+after > u.retryAfterMax {
				return nil, nil, &throttledError{after: after}
			}
			retriesTotal.WithLabelValues(u.name, retryReasonRateLimit).Inc()
			waited += after
			wait = after
		case attempt >= u.retries || !retryable(resp, err):
			if err != nil {
				cancel()
				return nil, nil, err
			}
			return resp, cancel, nil
		default:
			wait = u.backoffFor(attempt)
			reason := retryReasonError
			if resp != nil {
				reason = retryReason5xx
				// A 503 with Retry-After says when to come back; honour it
				// within the same budget as 429.
				if after, ok := retryAfter(resp.Header, u.now()); ok && waited+after <= u.retryAfterMax {
					waited += after
					wait = after
				}
				discard(resp)
			}
			cancel()
			retriesTotal.WithLabelValues(u.name, reason).Inc()
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, nil, err
		}
	}
}

// retryAfter parses a Retry-After header: delay-seconds or an HTTP date.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(min(secs, int64(24*time.Hour/time.Second))) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
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
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// attempt sends one request. On the remote's own origin it answers a Bearer
// token challenge once; related hosts never get the remote's credentials or
// a token obtained with them.
func (u *upstream) attempt(ctx context.Context, method string, target *url.URL, req core.UpstreamRequest, primary bool) (*http.Response, error) {
	var body io.Reader
	if req.Body != nil {
		body = bytes.NewReader(req.Body) // sets GetBody, so redirects and retries resend it
	}
	hreq, err := http.NewRequestWithContext(ctx, method, target.String(), body)
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
	if !primary {
		return u.client.Do(hreq)
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
	if hreq.GetBody != nil {
		if retry.Body, err = hreq.GetBody(); err != nil {
			return nil, fmt.Errorf("upstream request: %w", err)
		}
	}
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
