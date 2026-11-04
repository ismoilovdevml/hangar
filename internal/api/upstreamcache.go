package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// invalidateRepositoryCache handles POST /repositories/{name}/cache:invalidate:
// a proxy repository forgets the remote's remembered 404s and rate limits on
// every node, so the next request asks the remote again. Cached content is
// kept. Requires repository administration; audited.
func (a *API) invalidateRepositoryCache(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	if err := a.authorizeRepoAdmin(ctx, name, "repository.cache_invalidate"); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Deps.Repos == nil || a.UpstreamCache == nil {
		a.writeErr(w, r, notImplemented("proxy upstreams"))
		return
	}
	repo, err := a.Deps.Repos.Get(ctx, name)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if repo.Type != core.RepoProxy {
		a.writeErr(w, r, fmt.Errorf("repository %q is a %s repository, not a proxy: %w", name, repo.Type, core.ErrInvalid))
		return
	}
	res, err := a.UpstreamCache.InvalidateCache(ctx, repo)
	if a.Deps.Audit != nil {
		ev := core.AuditEvent{Action: "repository.cache_invalidate", Target: core.RepoResource(name).String(),
			Details: map[string]any{"negative_entries": res.NegativeEntries, "rate_limited_hosts": res.RateLimitedHosts}}
		if err != nil {
			ev.Outcome = core.OutcomeError
		}
		if aerr := a.Deps.Audit.Record(ctx, ev); aerr != nil && err == nil {
			err = aerr
		}
	}
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, res)
}
