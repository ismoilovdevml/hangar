package npm

import (
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// whoami answers `npm whoami` for token-authenticated clients.
func (h *handler) whoami(w http.ResponseWriter, r *http.Request) error {
	if !isRead(r.Method) {
		return errMethod
	}
	p := core.PrincipalFrom(r.Context())
	if p.IsAnonymous() {
		return fmt.Errorf("not logged in: %w", core.ErrUnauthenticated)
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": p.Name})
	return nil
}

// login implements the CouchDB-style login npm falls back to when web login
// is not offered: the body carries name and password, the answer a token.
// Hangar does not create users here (`npm adduser` for an unknown user fails).
func (h *handler) login(w http.ResponseWriter, r *http.Request, repo *core.Repository, pathUser string) error {
	if h.deps.Tokens == nil {
		return fmt.Errorf("token issuing is not configured: %w", core.ErrNotImplemented)
	}
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := readSmallJSON(r, &body); err != nil {
		return err
	}
	user, err := url.PathUnescape(pathUser)
	if err != nil {
		return fmt.Errorf("bad user name: %w", core.ErrInvalid)
	}
	if body.Name == "" {
		body.Name = user
	}
	if body.Name != user || body.Password == "" {
		return fmt.Errorf("name and password are required: %w", core.ErrInvalid)
	}
	ctx := r.Context()
	p, err := h.deps.Auth.Authenticate(ctx, core.Credentials{Username: body.Name, Password: body.Password})
	if err != nil {
		return err
	}
	if p.IsAnonymous() {
		return fmt.Errorf("incorrect username or password: %w", core.ErrUnauthenticated)
	}
	ctx = core.WithPrincipal(ctx, p)
	secret, exp, err := h.deps.Tokens.Issue(ctx, p, core.TokenRequest{
		TTL:  repoConfig(repo).loginTokenTTL(),
		Name: "npm login (" + repo.Name + ")",
	})
	if err != nil {
		return err
	}
	_ = h.deps.Audit.Record(ctx, core.AuditEvent{Action: "npm.login", Target: "repo:" + repo.Name,
		Details: map[string]any{"user": p.Name, "expires_at": exp}})
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "id": "org.couchdb.user:" + p.Name, "rev": "1", "token": secret,
	})
	return nil
}

// emptyQuickAudit is the response shape of /-/npm/v1/security/audits[/quick].
func emptyQuickAudit() map[string]any {
	return map[string]any{
		"actions":    []any{},
		"advisories": map[string]any{},
		"muted":      []any{},
		"metadata": map[string]any{
			"vulnerabilities": map[string]int{"info": 0, "low": 0, "moderate": 0, "high": 0, "critical": 0},
			"dependencies":    0, "devDependencies": 0, "optionalDependencies": 0, "totalDependencies": 0,
		},
	}
}

// audit answers npm's audit endpoints with "no advisories". Findings from
// Hangar's own scanner will be returned here once the security service
// exposes them to formats; the request body is not inspected.
func (h *handler) audit(w http.ResponseWriter, r *http.Request, repo *core.Repository, rest string) error {
	if r.Method != http.MethodPost {
		return errMethod
	}
	if err := h.authorize(r.Context(), core.ActionRead, repo); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<20))
	switch rest {
	case "advisories/bulk":
		writeJSON(w, http.StatusOK, map[string]any{})
	case "audits/quick", "audits":
		writeJSON(w, http.StatusOK, emptyQuickAudit())
	default:
		return fmt.Errorf("unknown audit endpoint: %w", core.ErrNotFound)
	}
	return nil
}
