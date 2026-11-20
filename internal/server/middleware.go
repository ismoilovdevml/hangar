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
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
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
	// proxies counts the trusted proxies in front of the peer (the
	// trusted hops passed over); the peer is one more.
	proxies := 0
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			break
		}
		info.RemoteIP = a.Unmap().String()
		if !trusted.contains(a) || i == 0 {
			break
		}
		proxies++
	}
	if p := forwardedValue(r.Header.Values("X-Forwarded-Proto"), proxies); p == "http" || p == "https" {
		info.Scheme = p
	}
	if h := forwardedValue(r.Header.Values("X-Forwarded-Host"), proxies); h != "" {
		info.Host = h
	}
	return info
}

// forwardedValue picks, from the comma-separated values of every line of an
// X-Forwarded-Proto or X-Forwarded-Host header, the one the outermost
// trusted proxy (the one the client talked to) wrote. Proxies append, so
// that is the entry `proxies` places left of the last one, as with
// X-Forwarded-For; entries further left came from the client and are never
// used. Proxies that replace the header instead of appending leave fewer
// entries, and the leftmost one present is then a proxy's own.
func forwardedValue(lines []string, proxies int) string {
	var vals []string
	for _, l := range lines {
		for v := range strings.SplitSeq(l, ",") {
			vals = append(vals, strings.ToLower(strings.TrimSpace(v)))
		}
	}
	if len(vals) == 0 {
		return ""
	}
	return vals[max(len(vals)-1-proxies, 0)]
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
