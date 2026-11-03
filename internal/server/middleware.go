package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/observability"
)

// HeaderRequestID carries the request id in and out.
const HeaderRequestID = "X-Request-ID"

// TrustedProxies is a set of networks whose forwarding headers are believed.
type TrustedProxies []netip.Prefix

// ParseTrustedProxies parses CIDRs or single IPs.
func ParseTrustedProxies(items []string) (TrustedProxies, error) {
	out := make(TrustedProxies, 0, len(items))
	for _, s := range items {
		s = strings.TrimSpace(s)
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: not a CIDR or IP", s)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

func (t TrustedProxies) contains(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range t {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// RequestInfo resolves the request id, client IP, scheme and host (honouring
// X-Forwarded-* only from trusted proxies) and stores them as core.RequestInfo.
func RequestInfo(trusted TrustedProxies) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderRequestID)
			if !validRequestID(id) {
				id = newRequestID()
			}
			w.Header().Set(HeaderRequestID, id)
			info := resolveClient(r, trusted)
			info.RequestID = id
			info.UserAgent = r.UserAgent()
			next.ServeHTTP(w, r.WithContext(core.WithRequestInfo(r.Context(), info)))
		})
	}
}

func resolveClient(r *http.Request, trusted TrustedProxies) core.RequestInfo {
	info := core.RequestInfo{Scheme: "http", Host: r.Host}
	if r.TLS != nil {
		info.Scheme = "https"
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	info.RemoteIP = host
	peer, err := netip.ParseAddr(host)
	if err != nil || len(trusted) == 0 || !trusted.contains(peer) {
		return info
	}
	// Walk X-Forwarded-For right to left; the first address that is not a
	// trusted proxy is the client. Everything left of it is client-supplied.
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		for p := range strings.SplitSeq(h, ",") {
			if p = strings.TrimSpace(p); p != "" {
				hops = append(hops, p)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			break
		}
		info.RemoteIP = a.Unmap().String()
		if !trusted.contains(a) {
			break
		}
	}
	if p := firstValue(r.Header.Get("X-Forwarded-Proto")); p == "http" || p == "https" {
		info.Scheme = p
	}
	if h := firstValue(r.Header.Get("X-Forwarded-Host")); h != "" {
		info.Host = h
	}
	return info
}

func firstValue(h string) string {
	v, _, _ := strings.Cut(h, ",")
	return strings.ToLower(strings.TrimSpace(v))
}

func validRequestID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// quietPaths are logged at debug level (probes and scrapes).
var quietPaths = map[string]bool{"/healthz": true, "/readyz": true, "/metrics": true}

// Observe logs one JSON line per request and records Prometheus metrics,
// labelled by chi route pattern (bounded cardinality).
func Observe(log *slog.Logger, m *observability.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			if m != nil {
				m.HTTPInFlight.Inc()
				defer m.HTTPInFlight.Dec()
			}
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			route := "unmatched"
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				route = rc.RoutePattern()
			}
			dur := time.Since(start)
			if m != nil {
				m.HTTPRequests.WithLabelValues(r.Method, route, strconv.Itoa(status)).Inc()
				m.HTTPDuration.WithLabelValues(r.Method, route).Observe(dur.Seconds())
				m.HTTPResponseLen.WithLabelValues(route).Add(float64(ww.BytesWritten()))
			}
			level := slog.LevelInfo
			switch {
			case status >= 500 && status != http.StatusNotImplemented:
				level = slog.LevelError
			case quietPaths[r.URL.Path]:
				level = slog.LevelDebug
			}
			info := core.RequestInfoFrom(r.Context())
			log.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", route),
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Int64("duration_ms", dur.Milliseconds()),
				slog.String("remote_ip", info.RemoteIP),
				slog.String("user_agent", info.UserAgent),
			)
		})
	}
}

// Recover turns a handler panic into a logged 500 instead of a dropped connection.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(rec)
				}
				log.ErrorContext(r.Context(), "handler panic", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":"internal","message":"internal server error"}` + "\n"))
			}()
			next.ServeHTTP(w, r)
		})
	}
}
