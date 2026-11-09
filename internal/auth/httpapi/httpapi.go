// Package httpapi serves the authentication and access-management part of
// the REST API (api/openapi.yaml): /auth/*, /me/*, /tokens, /users and /roles,
// plus the middleware that authenticates every /api/v1 request from a
// session cookie, a bearer token or HTTP Basic credentials, and enforces
// CSRF protection for cookie sessions.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// Organization is shown by GET /auth/session (schema Organization).
type Organization struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Environment string `json:"environment"`
	Region      string `json:"region"`
}

// Options configure the handlers.
type Options struct {
	Organization Organization
	// BaseURL is server.base_url; requests whose Origin matches it (or the
	// request's own scheme and host) pass the cross-origin check.
	BaseURL string
	// OIDC, when set, serves single sign-on at /auth/oidc/start and
	// /auth/oidc/callback.
	OIDC *oidc.Provider
	// OIDCSource, when set, replaces OIDC: it returns the provider in effect
	// (nil when single sign-on is off) for settings that change at run time.
	OIDCSource func() *oidc.Provider
	Logger     *slog.Logger
}

type handler struct {
	svc  *auth.Service
	cfg  auth.Config
	opts Options
	log  *slog.Logger
}

// oidc returns the single sign-on provider in effect, or nil.
func (h *handler) oidc() *oidc.Provider {
	if h.opts.OIDCSource != nil {
		return h.opts.OIDCSource()
	}
	return h.opts.OIDC
}

func newHandler(svc *auth.Service, opts Options) *handler {
	h := &handler{svc: svc, cfg: svc.Config(), opts: opts, log: opts.Logger}
	if h.log == nil {
		h.log = slog.Default()
	}
	return h
}

// Routes returns a router with the authentication middleware and every route
// of this package, with paths relative to /api/v1 (mount it there).
func Routes(svc *auth.Service, opts Options) chi.Router {
	r := chi.NewRouter()
	r.Use(Middleware(svc, opts))
	Register(r, svc, opts)
	return r
}

// Register adds this package's routes to r, which is already serving
// /api/v1 and has Middleware installed.
func Register(r chi.Router, svc *auth.Service, opts Options) {
	h := newHandler(svc, opts)
	r.Get("/auth/providers", h.providers)
	r.Post("/auth/login", h.login)
	r.Post("/auth/logout", h.logout)
	r.Get("/auth/session", h.session)
	r.Get("/me", h.session)
	r.Get("/auth/oidc/start", h.oidcStart)
	r.Get("/auth/oidc/callback", h.oidcCallback)
	r.Get("/identity-providers", h.identityProviders)

	r.Get("/tokens", h.listTokens)
	r.Post("/tokens", h.createToken)
	r.Delete("/tokens/{id}", h.revokeToken)
	r.Post("/tokens/{id}/rotate", h.rotateToken)

	r.Get("/users", h.listUsers)
	r.Post("/users", h.createUser)
	r.Get("/users/{id}", h.getUser)
	r.Put("/users/{id}", h.updateUser)
	r.Delete("/users/{id}", h.deleteUser)

	r.Post("/users/{id}/unlock", h.unlockUser)
	r.Post("/users/{id}/disable", h.disableUser)
	r.Post("/users/{id}/enable", h.enableUser)
	r.Put("/users/{id}/password", h.resetPassword)
	r.Post("/users/{id}/require-password-change", h.requirePasswordChange)
	r.Get("/users/{id}/permissions", h.userPermissions)
	r.Get("/users/{id}/sessions", h.userSessions)
	r.Delete("/users/{id}/sessions", h.revokeUserSessions)
	r.Delete("/users/{id}/sessions/{sid}", h.revokeUserSession)

	r.Put("/me/profile", h.updateProfile)
	r.Put("/me/password", h.changePassword)
	r.Get("/me/sessions", h.mySessions)
	r.Delete("/me/sessions", h.revokeMySessions)
	r.Delete("/me/sessions/{sid}", h.revokeMySession)

	r.Get("/roles", h.listRoles)
	r.Post("/roles", h.createRole)
	r.Get("/roles/{name}", h.getRole)
	r.Put("/roles/{name}", h.updateRole)
	r.Delete("/roles/{name}", h.deleteRole)
	r.Post("/roles/{name}/members", h.bindRole)
	r.Delete("/roles/{name}/members/{id}", h.unbindRole)
}

// ---------------------------------------------------------------- middleware

type ctxKey struct{}

// requestAuth is what Middleware learned about the request.
type requestAuth struct {
	session *auth.Session
	secret  string // session cookie value
}

func authFrom(ctx context.Context) *requestAuth {
	a, _ := ctx.Value(ctxKey{}).(*requestAuth)
	return a
}

// Middleware authenticates the request and stores the principal with
// core.WithPrincipal: an Authorization header (Bearer token, or Basic with a
// password or token) wins; otherwise the session cookie; otherwise the
// anonymous principal. Invalid header credentials answer 401 (423 while the
// account is locked). Mutating requests authenticated by the session cookie
// must carry the CSRF token header and, if the browser sent an Origin, come
// from this server's origin. A user who must change their password reaches
// only the routes of passwordChangeRoutes; everything else answers 403
// password_change_required. Running it twice is harmless.
func Middleware(svc *auth.Service, opts Options) func(http.Handler) http.Handler {
	h := newHandler(svc, opts)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if authFrom(ctx) != nil {
				next.ServeHTTP(w, r)
				return
			}
			ra := &requestAuth{}
			p := core.Anonymous()
			if creds := core.CredentialsFromRequest(r); !creds.Empty() {
				var err error
				if p, err = svc.AuthenticateRequest(ctx, creds); err != nil {
					if core.HTTPStatus(err) == http.StatusUnauthorized && !errors.As(err, new(*auth.LockedError)) {
						w.Header().Set("WWW-Authenticate", challenge)
					}
					h.writeError(w, r, err)
					return
				}
			} else if c, err := r.Cookie(h.cfg.Session.CookieName); err == nil && c.Value != "" {
				sp, sess, err := svc.SessionPrincipal(ctx, c.Value)
				switch {
				case errors.Is(err, core.ErrUnauthenticated):
					h.clearCookies(w, r)
				case err != nil:
					h.writeError(w, r, err)
					return
				default:
					p, ra.session, ra.secret = sp, sess, c.Value
				}
			}
			if ra.session != nil && !safeMethod(r.Method) {
				if !h.sameOrigin(r) {
					writeErrorBody(w, http.StatusForbidden, "forbidden", "cross-origin request rejected", 0)
					return
				}
				if !ra.session.ValidCSRF(r.Header.Get(h.cfg.Session.CSRFHeader)) {
					writeErrorBody(w, http.StatusForbidden, "csrf_failed",
						"missing or invalid "+h.cfg.Session.CSRFHeader+" header", 0)
					return
				}
			}
			if p.PasswordChangeRequired {
				switch passwordChangeRoute(r) {
				case routeBlocked:
					writeErrorBody(w, http.StatusForbidden, codePasswordChangeRequired,
						"set a new password before doing anything else (PUT /api/v1/me/password)", 0)
					return
				case routePublic:
					// Open to everyone anyway: answer as for a visitor.
					p = core.Anonymous()
				case routeOwn:
				}
			}
			ctx = context.WithValue(core.WithPrincipal(ctx, p), ctxKey{}, ra)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// challenge is the WWW-Authenticate value of 401 answers to requests whose
// Authorization header failed: Bearer, so browsers never open a password
// dialog for the web UI's requests.
const challenge = `Bearer realm="Hangar API"`

// codePasswordChangeRequired is the Error code for requests of a user who
// must change their password first.
const codePasswordChangeRequired = "password_change_required"

type routeKind int

const (
	// routeBlocked: answered with 403 password_change_required.
	routeBlocked routeKind = iota
	// routeOwn: served with the restricted principal.
	routeOwn
	// routePublic: needs no sign-in; served as anonymous.
	routePublic
)

// passwordChangeRoutes are the API routes a user who must change their
// password may call, relative to /api/v1.
var passwordChangeRoutes = map[string]routeKind{
	"GET /auth/session":        routeOwn,
	"GET /me":                  routeOwn,
	"PUT /me/password":         routeOwn,
	"POST /auth/logout":        routeOwn,
	"GET /auth/providers":      routePublic,
	"POST /auth/login":         routePublic,
	"GET /auth/oidc/start":     routePublic,
	"GET /auth/oidc/callback":  routePublic,
	"GET /system/status":       routePublic,
	"GET /system/version":      routePublic,
	"GET /openapi.yaml":        routePublic,
	"GET /openapi.json":        routePublic,
	"GET /automation-guide.md": routePublic,
}

func passwordChangeRoute(r *http.Request) routeKind {
	path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, apiPrefix), "/")
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	return passwordChangeRoutes[method+" "+path]
}

// apiPrefix is where the REST API is mounted.
const apiPrefix = "/api/v1"

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions || m == http.MethodTrace
}

// sameOrigin accepts requests without an Origin header (non-browser clients,
// same-origin GETs) and those whose Origin is this server's.
func (h *handler) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	o, err := url.Parse(origin)
	if err != nil || o.Host == "" {
		return false
	}
	if h.opts.BaseURL != "" {
		if b, err := url.Parse(h.opts.BaseURL); err == nil && strings.EqualFold(b.Scheme, o.Scheme) && strings.EqualFold(b.Host, o.Host) {
			return true
		}
	}
	info := core.RequestInfoFrom(r.Context())
	scheme, host := info.Scheme, info.Host
	if host == "" {
		host = r.Host
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	return strings.EqualFold(o.Scheme, scheme) && strings.EqualFold(o.Host, host)
}

// ---------------------------------------------------------------- cookies

func (h *handler) setCookies(w http.ResponseWriter, secret string, sess *auth.Session) {
	maxAge := 0 // browser-session cookie unless "remember me"
	var expires time.Time
	if sess.Remember {
		maxAge = int(time.Until(sess.ExpiresAt).Seconds())
		expires = sess.ExpiresAt
	}
	secure := !h.cfg.Session.Insecure
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is on unless auth.session.insecure is set for plain-HTTP development
		Name: h.cfg.Session.CookieName, Value: secret, Path: "/", MaxAge: maxAge, Expires: expires,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	// Readable by the web UI, which echoes it in the CSRF header.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: intentionally not HttpOnly (double-submit token for the UI)
		Name: h.cfg.Session.CSRFCookieName, Value: sess.CSRFToken, Path: "/", MaxAge: maxAge, Expires: expires,
		HttpOnly: false, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set(h.cfg.Session.CSRFHeader, sess.CSRFToken)
}

func (h *handler) clearCookies(w http.ResponseWriter, _ *http.Request) {
	secure := !h.cfg.Session.Insecure
	for _, name := range []string{h.cfg.Session.CookieName, h.cfg.Session.CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: expiring cookies; attributes mirror setCookies
			Name: name, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0),
			HttpOnly: name == h.cfg.Session.CookieName, Secure: secure, SameSite: http.SameSiteLaxMode,
		})
	}
}

// ---------------------------------------------------------------- helpers

// apiError is schema Error.
type apiError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

var errRateLimited = errors.New("too many requests")

type rateLimitedError struct{ wait time.Duration }

func (e *rateLimitedError) Error() string { return "too many login attempts" }
func (e *rateLimitedError) Unwrap() error { return errRateLimited }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErrorBody(w http.ResponseWriter, status int, code, msg string, retryAfter time.Duration) {
	e := apiError{Code: code, Message: msg}
	if retryAfter > 0 {
		secs := int((retryAfter + time.Second - 1) / time.Second)
		e.RetryAfter = secs
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	writeJSON(w, status, e)
}

// writeError maps service errors to the Error body.
func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var locked *auth.LockedError
	var limited *rateLimitedError
	var external *auth.ExternalError
	switch {
	case errors.Is(err, auth.ErrPasswordChangeRequired):
		writeErrorBody(w, http.StatusForbidden, codePasswordChangeRequired, err.Error(), 0)
	case errors.As(err, &external) && external.Code == auth.CodeAccountDisabled:
		writeErrorBody(w, http.StatusUnauthorized, "unauthenticated", "invalid username or password", 0)
	case errors.As(err, &external) && external.Code == auth.CodeAccountConflict:
		writeErrorBody(w, http.StatusConflict, "conflict", external.Message, 0)
	case errors.As(err, &external) && external.Code == auth.CodeInvalidProfile:
		writeErrorBody(w, http.StatusBadRequest, "invalid_argument", external.Message, 0)
	case errors.As(err, &external):
		writeErrorBody(w, http.StatusForbidden, "forbidden", external.Message, 0)
	case errors.Is(err, auth.ErrDirectoryUnavailable):
		h.log.WarnContext(r.Context(), "auth api: directory unavailable", "path", r.URL.Path, "error", err)
		writeErrorBody(w, http.StatusServiceUnavailable, "unavailable", "the directory cannot be reached; try again later", 0)
	case errors.As(err, &locked):
		writeErrorBody(w, http.StatusLocked, "locked", "account is locked after repeated failed sign-ins", locked.RetryAfter(h.svc.Now()))
	case errors.As(err, &limited):
		writeErrorBody(w, http.StatusTooManyRequests, "rate_limited", limited.Error(), limited.wait)
	case errors.Is(err, core.ErrUnauthenticated):
		writeErrorBody(w, http.StatusUnauthorized, "unauthenticated", err.Error(), 0)
	case errors.Is(err, core.ErrForbidden):
		writeErrorBody(w, http.StatusForbidden, "forbidden", err.Error(), 0)
	case errors.Is(err, core.ErrNotFound):
		writeErrorBody(w, http.StatusNotFound, "not_found", err.Error(), 0)
	case errors.Is(err, core.ErrAlreadyExists):
		writeErrorBody(w, http.StatusConflict, "conflict", err.Error(), 0)
	case errors.Is(err, core.ErrInvalid):
		writeErrorBody(w, http.StatusBadRequest, "invalid_argument", err.Error(), 0)
	case errors.Is(err, core.ErrNotImplemented):
		writeErrorBody(w, http.StatusNotImplemented, "not_implemented", err.Error(), 0)
	case errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled):
		// The client went away (navigated off, closed the connection): not a
		// server failure. 499 keeps it out of the server error counts.
		h.log.DebugContext(r.Context(), "auth api: request canceled by the client", "path", r.URL.Path, "error", err)
		writeErrorBody(w, statusClientClosedRequest, "canceled", "request canceled", 0)
	default:
		h.log.ErrorContext(r.Context(), "auth api: internal error", "path", r.URL.Path, "error", err)
		writeErrorBody(w, http.StatusInternalServerError, "internal", "internal server error", 0)
	}
}

// statusClientClosedRequest answers a request whose client went away first.
const statusClientClosedRequest = 499

const maxBody = 1 << 20

// decode reads a JSON body into v.
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			return fmt.Errorf("content type must be application/json: %w", core.ErrInvalid)
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON body: %s: %w", err.Error(), core.ErrInvalid)
	}
	return nil
}

func queryInt(r *http.Request, name string) (int, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > core.MaxPageSize {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d: %w", name, core.MaxPageSize, core.ErrInvalid)
	}
	return n, nil
}

func queryBool(r *http.Request, name string) (bool, error) {
	switch r.URL.Query().Get(name) {
	case "", "false", "0":
		return false, nil
	case "true", "1":
		return true, nil
	}
	return false, fmt.Errorf("%s must be true or false: %w", name, core.ErrInvalid)
}

func clientIP(r *http.Request) string {
	if ip := core.RequestInfoFrom(r.Context()).RemoteIP; ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// page is a cursor page in API form (next_cursor null on the last page).
type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func newPage[T any](items []T, next string) page[T] {
	p := page[T]{Items: items}
	if p.Items == nil {
		p.Items = []T{}
	}
	if next != "" {
		p.NextCursor = &next
	}
	return p
}
