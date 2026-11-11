package cargo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// credentials reads the Authorization header the way cargo sends it: the
// raw token (cargo login / CARGO_REGISTRIES_<NAME>_TOKEN), "Bearer <token>"
// (a token saved with that prefix), or HTTP Basic.
func credentials(r *http.Request) core.Credentials {
	if c := core.CredentialsFromRequest(r); !c.Empty() {
		return c
	}
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	if v == "" || strings.ContainsAny(v, " \t") {
		return core.Credentials{}
	}
	return core.Credentials{Token: v}
}

// authorize authenticates the request and checks action on repo. On failure
// it has already answered with a cargo error document: 401 (cargo then asks
// for `cargo login`) or 403.
func (h *handler) authorize(w http.ResponseWriter, r *http.Request, repo *core.Repository, action core.Action) (context.Context, bool) {
	ctx := r.Context()
	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" && credentials(r).Empty() {
		h.writeError(w, r, fmt.Errorf("unreadable Authorization header: %w", core.ErrUnauthenticated))
		return ctx, false
	}
	p, err := h.deps.Auth.Authenticate(ctx, credentials(r))
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

// errorDoc is the registry web API error body: cargo prints each detail.
type errorDoc struct {
	Errors []errorDetail `json:"errors"`
}

type errorDetail struct {
	Detail string `json:"detail"`
}

// writeError answers with {"errors":[{"detail": ...}]}.
func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if core.AnswerClientGone(w, r, h.log, err) {
		return
	}
	err = routing.Annotate(r.Context(), err)
	code := core.HTTPStatus(err)
	if errors.Is(err, errMethod) {
		code = http.StatusMethodNotAllowed
	}
	msg := err.Error()
	switch {
	case code == http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Cargo login_url="`+strings.TrimSuffix(h.deps.BaseURL, "/")+`/account"`)
		msg = "authentication required (missing, invalid or expired token): create a token in Hangar and run `cargo login --registry <name>`"
	case code == http.StatusForbidden && !errors.Is(err, core.ErrQuarantined):
		msg = "access denied"
	case code >= http.StatusInternalServerError && code != http.StatusBadGateway && code != http.StatusNotImplemented:
		h.log.ErrorContext(r.Context(), "cargo request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		msg = "internal error"
	case code == http.StatusBadGateway:
		h.log.WarnContext(r.Context(), "cargo upstream failed", "path", r.URL.Path, "err", err)
		msg = "upstream unavailable"
	case code == http.StatusTooManyRequests:
		h.log.WarnContext(r.Context(), "cargo upstream rate limited", "path", r.URL.Path, "err", err)
		msg = "upstream rate limited, retry later"
		core.SetRetryAfter(w.Header(), err)
	}
	writeJSON(w, code, errorDoc{Errors: []errorDetail{{Detail: msg}}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
