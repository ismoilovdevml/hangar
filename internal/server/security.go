package server

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Content security policies by route class. The web UI loads everything
// from its own origin; inline styles are allowed because the bundled
// components set style attributes. API responses are data, never documents.
// Package endpoints serve user-uploaded files from the UI's origin, so their
// documents are sandboxed (no scripts, opaque origin: no access to the
// session cookie); handlers may set a stricter policy of their own.
const (
	CSPWebUI = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
		"font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
	CSPAPI     = "default-src 'none'; frame-ancestors 'none'"
	CSPPackage = "default-src 'none'; img-src 'self' data:; style-src 'unsafe-inline'; frame-ancestors 'none'; sandbox"
)

// maxOtherBody bounds request bodies outside the API and package endpoints
// (web UI, health, metrics), which take none.
const maxOtherBody = 64 << 10

// Rate is a token bucket: Burst requests at once, refilled at PerMinute.
// PerMinute <= 0 disables it.
type Rate struct {
	PerMinute float64
	Burst     int
}

// Security configures the hardening middleware. The zero value only adds
// the static security headers.
type Security struct {
	// HSTSMaxAge enables Strict-Transport-Security on HTTPS requests.
	HSTSMaxAge            time.Duration
	HSTSIncludeSubdomains bool
	// BaseURL is server.base_url; its origin is always same-origin.
	BaseURL string
	// AllowedOrigins are extra browser origins allowed cross-origin access
	// (scheme://host[:port], lower case). Everything else is denied.
	AllowedOrigins []string
	// APIRate limits /api/v1 requests per client IP.
	APIRate Rate
	// FailedAuthRate limits failed authentications per client IP on the API
	// and package endpoints.
	FailedAuthRate Rate
	// APIBodyBytes bounds /api/v1 request bodies (0 = no limit).
	APIBodyBytes int64
	// UploadBodyBytes bounds package endpoint request bodies (0 = no limit).
	UploadBodyBytes int64
	// BodyIdleTimeout fails a body read that waits longer than this for data.
	BodyIdleTimeout time.Duration
	// Now is the clock (tests).
	Now func() time.Time
}

// hardening holds the middleware state built from Security.
type hardening struct {
	cfg        Security
	baseOrigin string
	allowed    map[string]bool
	apiLimit   *ipLimiter
	authFails  *ipLimiter
}

func newHardening(s Security) *hardening {
	h := &hardening{cfg: s, allowed: map[string]bool{}}
	if u, err := url.Parse(s.BaseURL); err == nil && u.Host != "" {
		h.baseOrigin = strings.ToLower(u.Scheme + "://" + u.Host)
	}
	for _, o := range s.AllowedOrigins {
		h.allowed[strings.ToLower(strings.TrimRight(o, "/"))] = true
	}
	h.apiLimit = newIPLimiter(s.APIRate, s.Now)
	h.authFails = newIPLimiter(s.FailedAuthRate, s.Now)
	return h
}

// headers sets the headers every response carries.
func (h *hardening) headers(next http.Handler) http.Handler {
	hsts := ""
	if h.cfg.HSTSMaxAge > 0 {
		hsts = "max-age=" + strconv.FormatInt(int64(h.cfg.HSTSMaxAge/time.Second), 10)
		if h.cfg.HSTSIncludeSubdomains {
			hsts += "; includeSubDomains"
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("X-Frame-Options", "DENY")
		if hsts != "" && core.RequestInfoFrom(r.Context()).Scheme == "https" {
			hdr.Set("Strict-Transport-Security", hsts)
		}
		next.ServeHTTP(w, r)
	})
}

// apiHeaders marks API responses as non-documents that must not be cached.
// Handlers may override Cache-Control.
func apiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", CSPAPI)
		hdr.Set("Cross-Origin-Resource-Policy", "same-origin")
		hdr.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// packageHeaders sandboxes documents served by package endpoints.
func packageHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", CSPPackage)
		next.ServeHTTP(w, r)
	})
}

// uiHeaders applies the web UI's content security policy.
func uiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", CSPWebUI)
		hdr.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// isAPI reports whether path is served by the REST API.
func isAPI(path string) bool { return path == "/api" || strings.HasPrefix(path, "/api/") }

// cors answers preflight requests and enforces the origin policy: same
// origin (server.base_url or the request's own origin) and AllowedOrigins
// pass; any other Origin is refused on the API and for every method except
// GET and HEAD. Package clients send no Origin and are unaffected.
func (h *hardening) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		norm := strings.ToLower(origin)
		same := norm == h.baseOrigin || norm == requestOrigin(r)
		allowed := !same && h.allowed[norm]
		w.Header().Add("Vary", "Origin")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if !same && !allowed {
				writeJSONError(w, http.StatusForbidden, "cors_denied", "cross-origin request from "+origin+" is not allowed", 0)
				return
			}
			hdr := w.Header()
			hdr.Set("Access-Control-Allow-Origin", origin)
			hdr.Set("Access-Control-Allow-Credentials", "true")
			hdr.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE")
			if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				hdr.Set("Access-Control-Allow-Headers", req)
			}
			hdr.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch {
		case allowed:
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		case !same && (isAPI(r.URL.Path) || (r.Method != http.MethodGet && r.Method != http.MethodHead)):
			writeJSONError(w, http.StatusForbidden, "cors_denied", "cross-origin request from "+origin+" is not allowed", 0)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestOrigin is the effective origin of r (trusted forwarding headers
// applied), lower case.
func requestOrigin(r *http.Request) string {
	info := core.RequestInfoFrom(r.Context())
	scheme, host := info.Scheme, info.Host
	if scheme == "" {
		scheme = "http"
	}
	if host == "" {
		host = r.Host
	}
	return strings.ToLower(scheme + "://" + host)
}

// bodyLimit bounds request bodies: limit bytes (0 = unlimited) and an idle
// timeout between reads. A declared Content-Length over the limit is refused
// with 413 before the handler runs; a longer chunked body fails the read.
func (h *hardening) bodyLimit(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body == nil || r.Body == http.NoBody {
				next.ServeHTTP(w, r)
				return
			}
			if limit > 0 && !streamedUpload(r) {
				if r.ContentLength > limit {
					writeJSONError(w, http.StatusRequestEntityTooLarge, "body_too_large",
						"request body larger than "+strconv.FormatInt(limit, 10)+" bytes", 0)
					return
				}
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}
			if d := h.cfg.BodyIdleTimeout; d > 0 {
				rc := http.NewResponseController(w)
				if rc.SetReadDeadline(time.Time{}) == nil {
					r.Body = &idleBody{ReadCloser: r.Body, rc: rc, idle: d}
					defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// streamedUpload reports whether an API request carries a file upload
// (multipart or raw bytes), which handlers stream and bound themselves.
func streamedUpload(r *http.Request) bool {
	if !isAPI(r.URL.Path) {
		return false
	}
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.HasPrefix(ct, "multipart/form-data") || strings.HasPrefix(ct, "application/octet-stream")
}

// idleBody extends the connection's read deadline before every read, so a
// body that keeps arriving is never cut off but one that stalls is.
type idleBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	done bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	if !b.done {
		_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	}
	n, err := b.ReadCloser.Read(p)
	if err != nil && !b.done {
		// At EOF the server starts its own background read on the
		// connection; it must not inherit the deadline.
		b.done = true
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

func (b *idleBody) Close() error { return b.ReadCloser.Close() }

// apiRateLimit limits /api/v1 requests per client IP.
func (h *hardening) apiRateLimit(next http.Handler) http.Handler {
	if h.apiLimit == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := h.apiLimit.take(clientKey(r)); !ok {
			writeRateLimited(w, wait, "too many requests")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// failedAuthLimit counts failed authentications (401 answers to requests
// that carried credentials) per client IP. Once a client IP has none left,
// its requests that carry credentials answer 429 without being checked, so
// passwords and tokens cannot be guessed at speed. Anonymous requests are
// never refused here.
func (h *hardening) failedAuthLimit(next http.Handler) http.Handler {
	if h.authFails == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !carriesCredentials(r) {
			next.ServeHTTP(w, r)
			return
		}
		key := clientKey(r)
		if blocked, wait := h.authFails.blocked(key); blocked {
			writeRateLimited(w, wait, "too many failed authentication attempts")
			return
		}
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if st := ww.Status(); st == http.StatusUnauthorized || st == http.StatusLocked {
			h.authFails.charge(key)
		}
	})
}

// carriesCredentials reports whether r may be an authentication attempt:
// an Authorization header, or a body (npm login, the registry token
// endpoint and similar take credentials in the body).
func carriesCredentials(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	}
	return false
}

// clientKey is the rate limiting key of r: the client IP, with IPv6
// addresses grouped by /64 (one site's allocation).
func clientKey(r *http.Request) string {
	ip := core.RequestInfoFrom(r.Context()).RemoteIP
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

func writeRateLimited(w http.ResponseWriter, wait time.Duration, msg string) {
	secs := max(int(math.Ceil(wait.Seconds())), 1)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeJSONError(w, http.StatusTooManyRequests, "rate_limited", msg, secs)
}

// writeJSONError writes the API error body (schema Error).
func writeJSONError(w http.ResponseWriter, status int, code, msg string, retryAfter int) {
	body := map[string]any{"code": code, "message": msg}
	if retryAfter > 0 {
		body["retry_after"] = retryAfter
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Del("Content-Length")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ipLimiter is a token bucket per key with a bounded number of keys.
type ipLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokens
	perSec  float64
	burst   float64
	now     func() time.Time
}

type tokens struct {
	n    float64
	last time.Time
}

// maxLimiterKeys bounds memory; full buckets are dropped first.
const maxLimiterKeys = 100_000

func newIPLimiter(r Rate, now func() time.Time) *ipLimiter {
	if r.PerMinute <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &ipLimiter{buckets: map[string]*tokens{}, perSec: r.PerMinute / 60, burst: float64(max(r.Burst, 1)), now: now}
}

// refill returns the bucket of key brought up to date. Callers hold mu.
func (l *ipLimiter) refill(key string) *tokens {
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxLimiterKeys {
			l.prune(now)
		}
		b = &tokens{n: l.burst, last: now}
		l.buckets[key] = b
		return b
	}
	b.n = math.Min(l.burst, b.n+now.Sub(b.last).Seconds()*l.perSec)
	b.last = now
	return b
}

func (l *ipLimiter) wait(b *tokens) time.Duration {
	return time.Duration((1 - b.n) / l.perSec * float64(time.Second))
}

// take consumes one token; without one it reports how long to wait.
func (l *ipLimiter) take(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key)
	if b.n < 1 {
		return false, l.wait(b)
	}
	b.n--
	return true, 0
}

// blocked reports, without consuming, whether key has no token left.
func (l *ipLimiter) blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key)
	if b.n < 1 {
		return true, l.wait(b)
	}
	return false, 0
}

// charge consumes one token if there is one.
func (l *ipLimiter) charge(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.refill(key); b.n >= 1 {
		b.n--
	}
}

func (l *ipLimiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if b.n+now.Sub(b.last).Seconds()*l.perSec >= l.burst {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= maxLimiterKeys {
		clear(l.buckets)
	}
}
