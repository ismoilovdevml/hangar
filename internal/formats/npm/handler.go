package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

type handler struct {
	f    *Format
	deps core.Deps
}

// errMethod answers 405.
var errMethod = errors.New("method not allowed")

// status maps an error to the HTTP status npm clients expect.
func status(err error) int {
	switch {
	case errors.Is(err, errMethod):
		return http.StatusMethodNotAllowed
	case errors.Is(err, errTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, errVersionExists), errors.Is(err, errVersionUnpublished), errors.Is(err, errUnpublishDenied):
		return http.StatusForbidden
	}
	return core.HTTPStatus(err)
}

// writeError sends the error body npm clients print: {"error": "..."}.
func writeError(w http.ResponseWriter, err error) {
	code := status(err)
	msg := err.Error()
	if code == http.StatusInternalServerError {
		msg = "internal server error"
	}
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="Hangar"`)
	}
	if code == http.StatusTooManyRequests {
		core.SetRetryAfter(w.Header(), err) // npm retries 429 after Retry-After
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", mediaJSON)
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func ok(w http.ResponseWriter, code int) {
	writeJSON(w, code, map[string]any{"ok": true})
}

// authenticate resolves the request principal and stores it in the context.
func (h *handler) authenticate(r *http.Request) (*http.Request, *core.Principal, error) {
	p, err := h.deps.Auth.Authenticate(r.Context(), core.CredentialsFromRequest(r))
	if err != nil {
		return r, nil, err
	}
	return r.WithContext(core.WithPrincipal(r.Context(), p)), p, nil
}

func (h *handler) authorize(ctx context.Context, action core.Action, repo *core.Repository) error {
	return h.deps.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), action, core.RepoResource(repo.Name))
}

// tarballBase is the absolute URL of repo's root, used for tarball links.
func (h *handler) tarballBase(repo *core.Repository) string {
	return strings.TrimSuffix(h.deps.BaseURL, "/") + "/repository/" + repo.Name
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	if repo == nil {
		writeError(w, fmt.Errorf("no repository: %w", core.ErrNotFound))
		return
	}
	r, _, err := h.authenticate(r)
	if err != nil {
		writeError(w, err)
		return
	}
	ctx, tally := proxy.WithCacheTally(r.Context())
	defer tally.Record(Name, r.Method)
	if repo.Type == core.RepoGroup && isRead(r.Method) {
		// Reads authorize repo:<group>:read; proxy members fill their cache on it.
		ctx = core.WithAccessVia(ctx, repo.Name)
	}
	r = r.WithContext(ctx)
	if err := h.route(w, r, repo); err != nil {
		if code := status(err); code >= 500 && code != http.StatusNotImplemented {
			h.deps.Logger.ErrorContext(r.Context(), "npm request failed", "repository", repo.Name,
				"method", r.Method, "path", r.URL.Path, "error", err)
		}
		writeError(w, err)
	}
}

func isRead(m string) bool { return m == http.MethodGet || m == http.MethodHead }

func (h *handler) route(w http.ResponseWriter, r *http.Request, repo *core.Repository) error {
	p, err := decodePath(chi.URLParam(r, "*"))
	if err != nil {
		return err
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		if !isRead(r.Method) {
			return errMethod
		}
		writeJSON(w, http.StatusOK, map[string]any{"db_name": repo.Name})
		return nil
	}
	if rest, ok := strings.CutPrefix(p, "-/"); ok {
		return h.routeSpecial(w, r, repo, rest)
	}
	name, segs, err := splitPackage(p)
	if err != nil {
		return err
	}
	switch {
	case len(segs) == 0:
		switch {
		case isRead(r.Method):
			return h.getPackument(w, r, repo, name)
		case r.Method == http.MethodPut:
			return h.putPackage(w, r, repo, name)
		}
		return errMethod
	case len(segs) == 2 && segs[0] == "-rev":
		switch r.Method {
		case http.MethodPut:
			return h.putPackage(w, r, repo, name)
		case http.MethodDelete:
			return h.unpublishPackage(w, r, repo, name)
		}
		return errMethod
	case segs[0] == "-" && len(segs) >= 2:
		file := strings.Join(segs[1:], "/")
		if r.Method == http.MethodDelete {
			if i := strings.Index(file, "/-rev/"); i >= 0 {
				file = file[:i]
			}
			return h.deleteTarball(w, r, repo, name, canonicalTarball(name, name.Full+"/-/"+file))
		}
		if !isRead(r.Method) {
			return errMethod
		}
		// "@s/n/-/@s/n-1.0.0.tgz" (older lockfiles) is the same tarball as
		// the conventional "@s/n/-/n-1.0.0.tgz".
		return h.getTarball(w, r, repo, canonicalTarball(name, name.Full+"/-/"+file))
	case len(segs) == 1:
		if !isRead(r.Method) {
			return errMethod
		}
		return h.getVersion(w, r, repo, name, segs[0])
	}
	return fmt.Errorf("unknown npm path %q: %w", p, core.ErrNotFound)
}

func (h *handler) routeSpecial(w http.ResponseWriter, r *http.Request, repo *core.Repository, rest string) error {
	switch {
	case rest == "ping":
		writeJSON(w, http.StatusOK, map[string]any{})
		return nil
	case rest == "whoami":
		return h.whoami(w, r)
	case strings.HasPrefix(rest, "user/org.couchdb.user:"):
		user := strings.TrimPrefix(rest, "user/org.couchdb.user:")
		user, _, _ = strings.Cut(user, "/")
		switch {
		case r.Method == http.MethodPut:
			return h.login(w, r, repo, user)
		case isRead(r.Method):
			p := core.PrincipalFrom(r.Context())
			if p.IsAnonymous() || p.Name != user {
				return fmt.Errorf("user %q: %w", user, core.ErrNotFound)
			}
			writeJSON(w, http.StatusOK, map[string]any{"_id": "org.couchdb.user:" + user, "name": user,
				"email": p.Email, "type": "user", "roles": []string{}})
			return nil
		}
		return errMethod
	case strings.HasPrefix(rest, "user/token/"):
		// npm logout. Session tokens cannot be revoked through the format
		// contract; they expire (login_token_ttl) or are revoked in the UI.
		if r.Method != http.MethodDelete {
			return errMethod
		}
		ok(w, http.StatusOK)
		return nil
	case strings.HasPrefix(rest, "package/"):
		return h.routeDistTags(w, r, repo, strings.TrimPrefix(rest, "package/"))
	case strings.HasPrefix(rest, "npm/v1/security/"):
		return h.audit(w, r, repo, strings.TrimPrefix(rest, "npm/v1/security/"))
	}
	// Includes -/v1/login: answering 404 makes npm fall back to the
	// username/password login (PUT /-/user/org.couchdb.user:<name>).
	return fmt.Errorf("unknown npm endpoint -/%s: %w", rest, core.ErrNotFound)
}

func (h *handler) routeDistTags(w http.ResponseWriter, r *http.Request, repo *core.Repository, rest string) error {
	pkgPath, tagPart, found := strings.Cut(rest, "/dist-tags")
	if !found {
		return fmt.Errorf("unknown package endpoint: %w", core.ErrNotFound)
	}
	name, err := parseName(pkgPath)
	if err != nil {
		return err
	}
	tag := strings.TrimPrefix(tagPart, "/")
	if tag == "" {
		if !isRead(r.Method) {
			return errMethod
		}
		return h.getDistTags(w, r, repo, name)
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return h.getDistTag(w, r, repo, name, tag)
	case http.MethodPut, http.MethodPost:
		return h.putDistTag(w, r, repo, name, tag)
	case http.MethodDelete:
		return h.deleteDistTag(w, r, repo, name, tag)
	}
	return errMethod
}

// readSmallJSON decodes a small request body.
func readSmallJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("decode request body: %w: %w", err, core.ErrInvalid)
	}
	return nil
}
