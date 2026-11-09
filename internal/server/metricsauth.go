package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// MetricsAuthOptions configure access to the Prometheus endpoint
// (metrics.auth and friends in the configuration).
type MetricsAuthOptions struct {
	// Mode is "none" (or empty), "bearer" or "basic".
	Mode        string
	BearerToken string
	Username    string
	Password    string
	// AllowedCIDRs limits the endpoint to these client networks (CIDRs or
	// IPs) in addition to Mode; empty allows every client.
	AllowedCIDRs []string
}

// MetricsAuth guards the Prometheus endpoint. Clients outside the allowed
// networks get 403; missing or wrong credentials get 401 with a
// WWW-Authenticate challenge. Secrets are compared in constant time.
type MetricsAuth struct {
	mode    string
	token   [sha256.Size]byte
	user    [sha256.Size]byte
	pass    [sha256.Size]byte
	allowed TrustedProxies
}

// NewMetricsAuth validates o. It returns nil (no protection) for mode none
// without an allowlist.
func NewMetricsAuth(o MetricsAuthOptions) (*MetricsAuth, error) {
	a := &MetricsAuth{mode: strings.ToLower(strings.TrimSpace(o.Mode))}
	switch a.mode {
	case "", "none":
		a.mode = "none"
	case "bearer":
		if o.BearerToken == "" {
			return nil, errors.New("metrics auth bearer: empty token")
		}
		a.token = sha256.Sum256([]byte(o.BearerToken))
	case "basic":
		if o.Username == "" || o.Password == "" {
			return nil, errors.New("metrics auth basic: username and password are required")
		}
		a.user = sha256.Sum256([]byte(o.Username))
		a.pass = sha256.Sum256([]byte(o.Password))
	default:
		return nil, fmt.Errorf("metrics auth %q: want none, bearer or basic", o.Mode)
	}
	allowed, err := ParseTrustedProxies(o.AllowedCIDRs)
	if err != nil {
		return nil, fmt.Errorf("metrics allowed_cidrs: %w", err)
	}
	a.allowed = allowed
	if a.mode == "none" && len(a.allowed) == 0 {
		return nil, nil //nolint:nilnil // nil means the endpoint is open
	}
	return a, nil
}

// RequiresCredentials reports whether clients must authenticate (bearer or
// basic), as opposed to an allowlist only.
func (a *MetricsAuth) RequiresCredentials() bool { return a != nil && a.mode != "none" }

// Wrap returns next guarded by a; a nil MetricsAuth returns next unchanged.
func (a *MetricsAuth) Wrap(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(a.allowed) > 0 && !a.clientAllowed(r) {
			metricsDenied(w, http.StatusForbidden, "")
			return
		}
		switch a.mode {
		case "bearer":
			if !a.bearerOK(r) {
				metricsDenied(w, http.StatusUnauthorized, `Bearer realm="metrics"`)
				return
			}
		case "basic":
			if !a.basicOK(r) {
				metricsDenied(w, http.StatusUnauthorized, `Basic realm="metrics", charset="UTF-8"`)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// clientAllowed checks the client IP as resolved by RequestInfo, which
// honours X-Forwarded-For only from trusted proxies.
func (a *MetricsAuth) clientAllowed(r *http.Request) bool {
	ip, err := netip.ParseAddr(core.RequestInfoFrom(r.Context()).RemoteIP)
	return err == nil && a.allowed.contains(ip)
}

func (a *MetricsAuth) bearerOK(r *http.Request) bool {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return equalSum(a.token, strings.TrimSpace(tok))
}

func (a *MetricsAuth) basicOK(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	// Both halves are always compared, so timing does not reveal which failed.
	userOK := equalSum(a.user, user)
	passOK := equalSum(a.pass, pass)
	return userOK && passOK
}

// equalSum compares v with a stored SHA-256 sum in constant time; hashing
// first also hides the length of the secret.
func equalSum(want [sha256.Size]byte, v string) bool {
	got := sha256.Sum256([]byte(v))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

func metricsDenied(w http.ResponseWriter, status int, challenge string) {
	if challenge != "" {
		w.Header().Set("WWW-Authenticate", challenge)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, http.StatusText(status))
}
