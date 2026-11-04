package raw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// authRealm is the HTTP Basic realm in 401 challenges. curl and wget send
// credentials after the challenge (wget only then, unless --auth-no-challenge).
const authRealm = "Hangar"

func (h *handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	if repo == nil {
		h.fail(w, r, fmt.Errorf("raw handler called without a repository: %w", errInternal))
		return
	}
	allowed := allowedMethods(repo.Type)
	if !slices.Contains(allowed, r.Method) {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf("%s is not supported on a %s raw repository", r.Method, repo.Type))
		return
	}
	reqPath, err := requestPath(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	clean, dir, err := normalizePath(reqPath)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	action := core.ActionRead
	switch r.Method {
	case http.MethodPut:
		action = core.ActionWrite
	case http.MethodDelete:
		action = core.ActionDelete
	}
	ctx, err := h.authorize(r.Context(), r, repo, action)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	r = r.WithContext(ctx)

	switch r.Method {
	case http.MethodPut:
		if dir {
			h.fail(w, r, fmt.Errorf("cannot upload to a directory path; name the file: %w", core.ErrInvalid))
			return
		}
		h.put(w, r, repo, clean)
	case http.MethodDelete:
		if dir {
			h.fail(w, r, fmt.Errorf("cannot delete a directory; delete its files: %w", core.ErrInvalid))
			return
		}
		h.delete(w, r, repo, clean)
	default: // GET, HEAD
		if dir {
			h.list(w, r, repo, clean)
			return
		}
		h.get(w, r, repo, clean)
	}
}

// allowedMethods lists the methods a repository type answers.
func allowedMethods(t core.RepoType) []string {
	switch t {
	case core.RepoHosted:
		return []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete}
	case core.RepoProxy:
		return []string{http.MethodGet, http.MethodHead, http.MethodDelete}
	default:
		return []string{http.MethodGet, http.MethodHead}
	}
}

// authorize authenticates the request and checks action on repo. The
// returned context carries the principal and, for a read of a group, the
// group the members are read through (core.WithAccessVia), so proxy members
// fill their cache on the strength of the group's read privilege.
func (h *handler) authorize(ctx context.Context, r *http.Request, repo *core.Repository, action core.Action) (context.Context, error) {
	p, err := h.deps.Auth.Authenticate(ctx, core.CredentialsFromRequest(r))
	if err != nil {
		return ctx, err
	}
	ctx = core.WithPrincipal(ctx, p)
	if err := h.deps.AuthZ.Authorize(ctx, p, action, core.RepoResource(repo.Name)); err != nil {
		return ctx, err
	}
	if action == core.ActionRead && repo.Type == core.RepoGroup {
		ctx = core.WithAccessVia(ctx, repo.Name)
	}
	return ctx, nil
}

// errInternal marks failures that must not leak details to the client.
var errInternal = errors.New("internal error")

// fail answers with the status for err. 401 carries a Basic challenge; server
// errors are logged and answered without details.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	code := core.HTTPStatus(err)
	msg := err.Error()
	switch {
	case code == http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Basic realm="`+authRealm+`", charset="UTF-8"`)
		msg = "authentication required"
	case code == http.StatusBadGateway:
		h.log(r).Warn("raw: upstream failure", "path", r.URL.Path, "err", err)
		msg = "upstream unavailable"
	case code == http.StatusTooManyRequests:
		h.log(r).Warn("raw: upstream rate limited", "path", r.URL.Path, "err", err)
		msg = "upstream rate limited, retry later"
		core.SetRetryAfter(w.Header(), err)
	case code >= http.StatusInternalServerError:
		h.log(r).Error("raw: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		msg = "internal error"
	}
	writeError(w, code, msg)
}

func (h *handler) log(r *http.Request) *slog.Logger {
	if id := core.RequestInfoFrom(r.Context()).RequestID; id != "" {
		return h.deps.Logger.With("request_id", id)
	}
	return h.deps.Logger
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
