package npm

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

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
		// Marks the token as npm login's own, which npm logout may revoke.
		Extra: map[string]any{claimNPMLogin: repo.Name},
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

// claimNPMLogin is the token claim that marks tokens minted by npm login;
// its value is the repository logged in to.
const claimNPMLogin = "npm_login"

// logout answers npm logout (DELETE /-/user/token/<token>): the token is
// revoked when npm login minted it. Other tokens (personal access tokens
// pasted into .npmrc, tokens of other clients) are left alone, as their
// owner manages them in Hangar; npm forgets its copy either way, so the
// answer is 200 both times.
func (h *handler) logout(w http.ResponseWriter, r *http.Request, repo *core.Repository, secret string) error {
	if r.Method != http.MethodDelete {
		return errMethod
	}
	ctx := r.Context()
	if secret == "" || strings.Contains(secret, "/") {
		return fmt.Errorf("bad token: %w", core.ErrInvalid)
	}
	// Holding the token is the proof: it is authenticated as itself.
	p, err := h.deps.Auth.Authenticate(ctx, core.Credentials{Token: secret})
	if err != nil {
		if errors.Is(err, core.ErrUnauthenticated) {
			ok(w, http.StatusOK) // expired or already revoked
			return nil
		}
		return err
	}
	if _, minted := p.Claims[claimNPMLogin]; !minted || p.TokenID == "" {
		h.deps.Logger.InfoContext(ctx, "npm logout left a token that npm login did not mint", "repository", repo.Name, "user", p.Name)
		ok(w, http.StatusOK)
		return nil
	}
	rv, can := h.deps.Tokens.(core.TokenRevoker)
	if !can {
		return fmt.Errorf("token revocation is not configured: %w", core.ErrNotImplemented)
	}
	ctx = core.WithPrincipal(ctx, p)
	if err := rv.RevokeIssued(ctx, p.TokenID); err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	_ = h.deps.Audit.Record(ctx, core.AuditEvent{Action: "npm.logout", Target: "repo:" + repo.Name,
		Details: map[string]any{"user": p.Name, "token_id": p.TokenID}})
	ok(w, http.StatusOK)
	return nil
}
