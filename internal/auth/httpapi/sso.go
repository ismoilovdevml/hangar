package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/oidc"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// ---------------------------------------------------------------- /auth/oidc

// oidcStart sends the browser to the identity provider. The optional
// redirect query parameter is the in-app path to return to.
func (h *handler) oidcStart(w http.ResponseWriter, r *http.Request) {
	provider := h.oidc()
	if provider == nil {
		writeErrorBody(w, http.StatusNotFound, "not_found", "single sign-on is not enabled", 0)
		return
	}
	authURL, cookie, err := provider.Begin(r.Context(), r.URL.Query().Get("redirect"))
	if err != nil {
		h.log.WarnContext(r.Context(), "auth: oidc sign-in cannot start", "error", err)
		h.loginFailed(w, r, oidc.CodeUnavailable)
		return
	}
	h.setStateCookie(w, cookie, int(oidc.StateTTL/time.Second))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, authURL, http.StatusFound) //nolint:gosec // G710: authorization endpoint from the configured provider's discovery document
}

// oidcCallback finishes the sign-in the provider redirected back from and
// starts a session. Every failure lands on the login page with ?error=code.
func (h *handler) oidcCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	provider := h.oidc()
	if provider == nil {
		writeErrorBody(w, http.StatusNotFound, "not_found", "single sign-on is not enabled", 0)
		return
	}
	h.setStateCookie(w, "", -1) // single use
	if ok, _ := h.svc.LoginLimiter().Allow(clientIP(r)); !ok {
		h.loginFailed(w, r, "rate_limited")
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		h.svc.RecordLoginFailure(ctx, "", auth.MethodOIDC, "provider_error:"+truncate(e, 64))
		code := oidc.CodeFailed
		if e == "access_denied" {
			code = oidc.CodeDenied
		}
		h.loginFailed(w, r, code)
		return
	}
	var cookie string
	if c, err := r.Cookie(oidc.CookieName); err == nil {
		cookie = c.Value
	}
	id, redirect, err := provider.Complete(ctx, cookie, q.Get("state"), q.Get("code"))
	if err != nil {
		code, reason := oidc.CodeFailed, "internal"
		var oe *oidc.Error
		if errors.As(err, &oe) {
			code, reason = oe.Code, oe.Reason
		}
		h.log.WarnContext(ctx, "auth: oidc sign-in failed", "reason", reason, "error", err)
		h.svc.RecordLoginFailure(ctx, "", auth.MethodOIDC, reason)
		h.loginFailed(w, r, code)
		return
	}
	p, err := h.svc.SignInExternal(ctx, *id, auth.MethodOIDC)
	if err != nil {
		code := oidc.CodeFailed
		var xe *auth.ExternalError
		if errors.As(err, &xe) {
			code = xe.Code
		} else {
			h.log.ErrorContext(ctx, "auth: oidc sign-in failed", "username", id.Username, "error", err)
		}
		h.loginFailed(w, r, code)
		return
	}
	if err := h.startSession(w, r, p, false); err != nil {
		h.log.ErrorContext(ctx, "auth: oidc session", "error", err)
		h.loginFailed(w, r, oidc.CodeFailed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, oidc.SafeRedirect(redirect), http.StatusFound) //nolint:gosec // G710: SafeRedirect admits same-origin paths only
}

// loginFailed returns the browser to the login page with an error code.
func (h *handler) loginFailed(w http.ResponseWriter, r *http.Request, code string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/login?error="+url.QueryEscape(code), http.StatusFound)
}

func (h *handler) setStateCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is on unless auth.session.insecure is set for plain-HTTP development
		Name: oidc.CookieName, Value: value, Path: oidc.CookiePath, MaxAge: maxAge,
		HttpOnly: true, Secure: !h.cfg.Session.Insecure, SameSite: http.SameSiteLaxMode,
	})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---------------------------------------------------------------- /identity-providers

// identityProvider is schema IdentityProvider.
type identityProvider struct {
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	Enabled    bool       `json:"enabled"`
	State      string     `json:"state"`
	LastSyncAt *time.Time `json:"last_sync_at"`
	Error      string     `json:"error,omitempty"`
}

// Values of IdentityProvider.state.
const (
	providerOK       = "ok"
	providerError    = "error"
	providerDisabled = "disabled"
)

func (h *handler) identityProviders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := h.svc.Authorizer().Authorize(ctx, core.PrincipalFrom(ctx), core.ActionRead, core.SystemResource(core.SystemUsers)); err != nil {
		h.writeError(w, r, err)
		return
	}
	o := identityProvider{Name: auth.SourceOIDC, Kind: auth.SourceOIDC, State: providerDisabled}
	if provider := h.oidc(); provider != nil {
		o.Enabled, o.State = true, providerOK
		if ok, err := provider.Status(); !ok && err != nil {
			o.State, o.Error = providerError, err.Error()
		}
	}
	l := identityProvider{Name: auth.SourceLDAP, Kind: auth.SourceLDAP, State: providerDisabled}
	if dir := h.svc.Directory(); dir != nil {
		l.Enabled, l.State = true, providerOK
		if sr, ok := dir.(interface{ Status() (time.Time, error) }); ok {
			at, err := sr.Status()
			if !at.IsZero() {
				l.LastSyncAt = &at
			}
			if err != nil {
				l.State, l.Error = providerError, err.Error()
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": []identityProvider{o, l}})
}
