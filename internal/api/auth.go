package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// authenticate is the fallback authentication when no AuthMiddleware is
// wired: Basic or Bearer credentials through Deps.Auth, anonymous otherwise.
// Browser sessions need AuthMiddleware.
func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		creds := core.CredentialsFromRequest(r)
		if creds.Empty() {
			next.ServeHTTP(w, r.WithContext(core.WithPrincipal(r.Context(), core.Anonymous())))
			return
		}
		if a.Deps.Auth == nil {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "authentication is not configured on this server")
			return
		}
		p, err := a.Deps.Auth.Authenticate(r.Context(), creds)
		if err != nil {
			if errors.Is(err, core.ErrUnauthenticated) {
				WriteError(w, http.StatusUnauthorized, "unauthenticated", "invalid credentials")
				return
			}
			a.writeErr(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(core.WithPrincipal(r.Context(), p)))
	})
}

var errSignIn = fmt.Errorf("sign in required: %w", core.ErrUnauthenticated)

// PublicRoutes are the only operations anonymous callers may use: those
// documented with `security: []` in api/openapi.yaml (a test keeps the two
// in sync). Keys are "METHOD /path" relative to Prefix.
var PublicRoutes = map[string]bool{
	"GET /system/status":      true,
	"GET /system/version":     true,
	"GET /auth/providers":     true,
	"POST /auth/login":        true,
	"GET /auth/oidc/start":    true,
	"GET /auth/oidc/callback": true,
	"GET /openapi.yaml":       true,
	"GET /openapi.json":       true,
	"GET /automation-guide.md":     true,
}

// requireSignIn answers 401 to anonymous callers of every route that is not
// in PublicRoutes, before any handler (or the not-implemented fallback) runs.
func requireSignIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if core.PrincipalFrom(r.Context()).IsAnonymous() && !PublicRoutes[r.Method+" "+strings.TrimPrefix(r.URL.Path, Prefix)] {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refuseReadOnlyWrites answers 403 read_only_token to read-only tokens on
// every request that is not GET or HEAD. Their scopes are already narrowed to
// read, which covers every authorized action; this also stops the actions
// that need no privilege (own tokens, own profile).
func refuseReadOnlyWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if core.PrincipalFrom(r.Context()).ReadOnly && r.Method != http.MethodGet && r.Method != http.MethodHead {
			WriteError(w, http.StatusForbidden, "read_only_token", "this token is read-only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireUser fails for the anonymous principal.
func requireUser(ctx context.Context) error {
	if core.PrincipalFrom(ctx).IsAnonymous() {
		return errSignIn
	}
	return nil
}

// authorize checks the context principal. Without an AuthZ everything is
// refused (fail closed).
func (a *API) authorize(ctx context.Context, action core.Action, res core.Resource) error {
	if a.Deps.AuthZ == nil {
		return fmt.Errorf("authorization is not configured: %w", core.ErrNotImplemented)
	}
	return a.Deps.AuthZ.Authorize(ctx, core.PrincipalFrom(ctx), action, res)
}

// authorizeMutation is authorize for writes: a refusal is audited with
// outcome "denied" under auditAction.
func (a *API) authorizeMutation(ctx context.Context, action core.Action, res core.Resource, auditAction, target string) error {
	err := a.authorize(ctx, action, res)
	if err != nil && (errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrUnauthenticated)) && a.Deps.Audit != nil {
		_ = a.Deps.Audit.Record(ctx, core.AuditEvent{Action: auditAction, Target: target, Outcome: core.OutcomeDenied,
			Details: map[string]any{"reason": err.Error()}})
	}
	return err
}

// authorizeRepoAdmin follows RepositoryService: system:repos:admin or repo:<name>:admin.
func (a *API) authorizeRepoAdmin(ctx context.Context, name, auditAction string) error {
	if err := a.authorize(ctx, core.ActionAdmin, core.SystemResource(core.SystemRepos)); err == nil {
		return nil
	}
	return a.authorizeMutation(ctx, core.ActionAdmin, core.RepoResource(name), auditAction, core.RepoResource(name).String())
}

// canRead reports whether the principal may read repository name. Errors
// other than a refusal are returned.
func (a *API) canRead(ctx context.Context, name string) (bool, error) {
	err := a.authorize(ctx, core.ActionRead, core.RepoResource(name))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, core.ErrForbidden), errors.Is(err, core.ErrUnauthenticated):
		return false, nil
	default:
		return false, err
	}
}

// allRepositories pages through every repository, ordered by name.
func (a *API) allRepositories(ctx context.Context) ([]*core.Repository, error) {
	if a.Deps.Repos == nil {
		return nil, fmt.Errorf("repository service is not configured: %w", core.ErrNotImplemented)
	}
	var out []*core.Repository
	q := core.RepositoryQuery{Limit: core.MaxPageSize}
	for {
		p, err := a.Deps.Repos.List(ctx, q)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.NextCursor == "" {
			break
		}
		q.Cursor = p.NextCursor
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// readableRepos returns the repositories the principal may read.
func (a *API) readableRepos(ctx context.Context) ([]*core.Repository, error) {
	all, err := a.allRepositories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*core.Repository, 0, len(all))
	for _, r := range all {
		ok, err := a.canRead(ctx, r.Name)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// readableRepoIDs is readableRepos as an id allow-list (never nil).
func (a *API) readableRepoIDs(ctx context.Context) ([]string, error) {
	repos, err := a.readableRepos(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(repos))
	for _, r := range repos {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// notImplemented builds the 501 error for an unwired dependency.
func notImplemented(what string) error {
	return fmt.Errorf("%s is not available on this server: %w", what, core.ErrNotImplemented)
}
