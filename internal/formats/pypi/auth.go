package pypi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// authorize authenticates the request and checks action on repo. On failure
// it has already answered: 401 with a Basic challenge (pip and twine then
// use their configured credentials or keyring) or 403.
func (h *handler) authorize(w http.ResponseWriter, r *http.Request, repo *core.Repository, action core.Action) (context.Context, bool) {
	ctx := r.Context()
	p, err := h.deps.Auth.Authenticate(ctx, core.CredentialsFromRequest(r))
	if err != nil {
		h.writeError(w, r, err)
		return ctx, false
	}
	ctx = core.WithPrincipal(ctx, p)
	if err := h.deps.AuthZ.Authorize(ctx, p, action, core.RepoResource(repo.Name)); err != nil {
		h.writeError(w, r, err)
		return ctx, false
	}
	if action == core.ActionRead && repo.Type == core.RepoGroup {
		// Proxy members fill their cache on the group's read privilege.
		ctx = core.WithAccessVia(ctx, repo.Name)
	}
	return ctx, true
}

// writeError answers with a plain-text message, which pip and twine print.
func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if core.AnswerClientGone(w, r, h.log, err) {
		return
	}
	err = routing.Annotate(r.Context(), err)
	code := core.HTTPStatus(err)
	msg := err.Error()
	switch {
	case code == http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Basic realm="hangar"`)
		msg = "authentication required"
	case code == http.StatusForbidden && !errors.Is(err, core.ErrQuarantined):
		msg = "access denied"
	case code >= http.StatusInternalServerError && code != http.StatusBadGateway && code != http.StatusNotImplemented:
		h.log.ErrorContext(r.Context(), "pypi request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		msg = "internal error"
	case code == http.StatusBadGateway:
		h.log.WarnContext(r.Context(), "pypi upstream failed", "path", r.URL.Path, "err", err)
		msg = "upstream unavailable"
	case code == http.StatusTooManyRequests:
		h.log.WarnContext(r.Context(), "pypi upstream rate limited", "path", r.URL.Path, "err", err)
		msg = "upstream rate limited, retry later"
		core.SetRetryAfter(w.Header(), err)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = fmt.Fprintln(w, msg) //nolint:gosec // text/plain with nosniff, never rendered as HTML
}
