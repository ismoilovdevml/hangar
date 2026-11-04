package httpapi

import (
	"context"
	"net/http"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// sessionPrincipalKey carries the principal of a browser session resolved by
// DownloadSessions for package routes.
type sessionPrincipalKey struct{}

// DownloadSessions lets signed-in web UI users download packages from the
// format routes (/repository/..., /v2/...) with their session cookie.
//
// It only resolves the cookie: format handlers keep authenticating clients
// themselves, so Basic and Bearer credentials and each protocol's own
// challenge (WWW-Authenticate for OCI, npm's 401 JSON, ...) are unchanged.
// The cookie is honoured only for safe methods (GET, HEAD) and only when the
// request carries no Authorization header; uploads and deletes need real
// client credentials, so a cross-site form can never write with a browser's
// session. An unknown or expired cookie is ignored (anonymous), never an
// error. Pair it with SessionAuthenticator around the formats' Authenticator.
func DownloadSessions(svc *auth.Service) func(http.Handler) http.Handler {
	cookie := svc.Config().Session.CookieName
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("Authorization") != "" {
				next.ServeHTTP(w, r)
				return
			}
			c, err := r.Cookie(cookie)
			if err != nil || c.Value == "" {
				next.ServeHTTP(w, r)
				return
			}
			p, _, err := svc.SessionPrincipal(r.Context(), c.Value)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionPrincipalKey{}, p)))
		})
	}
}

// SessionAuthenticator wraps a: when a request presents no credentials but
// DownloadSessions resolved a browser session, Authenticate returns that
// session's principal instead of the anonymous one. Presented credentials are
// always checked by a.
func SessionAuthenticator(a core.Authenticator) core.Authenticator {
	return sessionAuthenticator{a}
}

type sessionAuthenticator struct{ core.Authenticator }

func (s sessionAuthenticator) Authenticate(ctx context.Context, c core.Credentials) (*core.Principal, error) {
	if c.Empty() {
		if p, ok := ctx.Value(sessionPrincipalKey{}).(*core.Principal); ok && p != nil {
			return p, nil
		}
	}
	return s.Authenticator.Authenticate(ctx, c)
}
