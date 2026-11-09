package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// Routing rules are part of the repository configuration (config.routing),
// so config-as-code and PUT /repositories/{name} carry them too; these
// endpoints edit them on their own, test a name and suggest patterns.

// maxSuggestComponents bounds the components read per hosted repository
// when suggesting patterns.
const maxSuggestComponents = 5000

// routingJSON is schema RoutingRules: lists are never null.
type routingJSON struct {
	Allow   []string            `json:"allow"`
	Block   []string            `json:"block"`
	Members []memberRoutingJSON `json:"members"`
}

type memberRoutingJSON struct {
	Member    string   `json:"member"`
	Exclusive []string `json:"exclusive"`
	Block     []string `json:"block"`
}

func toRoutingJSON(rt *core.RoutingRules) routingJSON {
	out := routingJSON{Allow: []string{}, Block: []string{}, Members: []memberRoutingJSON{}}
	if rt == nil {
		return out
	}
	out.Allow = append(out.Allow, rt.Allow...)
	out.Block = append(out.Block, rt.Block...)
	for _, m := range rt.Members {
		out.Members = append(out.Members, memberRoutingJSON{Member: m.Member,
			Exclusive: append([]string{}, m.Exclusive...), Block: append([]string{}, m.Block...)})
	}
	return out
}

// routingRepo resolves {name} for a routing endpoint: a proxy or a group the
// caller may read.
func (a *API) routingRepo(ctx context.Context, name string) (*core.Repository, error) {
	repo, err := a.getReadableRepo(ctx, name)
	if err != nil {
		return nil, err
	}
	if repo.Type != core.RepoProxy && repo.Type != core.RepoGroup {
		return nil, fmt.Errorf("repository %q is a %s repository; routing rules apply to proxy and group repositories: %w", name, repo.Type, core.ErrInvalid)
	}
	return repo, nil
}

func (a *API) getRouting(w http.ResponseWriter, r *http.Request) {
	repo, err := a.routingRepo(r.Context(), chi.URLParam(r, "name"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toRoutingJSON(repo.Config.Routing))
}

func (a *API) putRouting(w http.ResponseWriter, r *http.Request) {
	var in core.RoutingRules
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	a.saveRouting(w, r, &in)
}

func (a *API) deleteRouting(w http.ResponseWriter, r *http.Request) {
	a.saveRouting(w, r, nil)
}

// saveRouting replaces the repository's routing rules (nil clears them).
func (a *API) saveRouting(w http.ResponseWriter, r *http.Request, rules *core.RoutingRules) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	if err := a.authorizeRepoAdmin(ctx, name, "repository.routing.update"); err != nil {
		a.writeErr(w, r, err)
		return
	}
	repo, err := a.routingRepo(ctx, name)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	if rules.Empty() {
		rules = nil
	}
	before := repo.Config.Routing
	repo.Config.Routing = rules
	updated, err := a.Deps.Repos.Update(ctx, repo)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	a.record(ctx, "repository.routing.update", core.RepoResource(name).String(), map[string]any{
		"before": toRoutingJSON(before), "after": toRoutingJSON(updated.Config.Routing)})
	if rules == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	WriteJSON(w, http.StatusOK, toRoutingJSON(updated.Config.Routing))
}

// testRouting evaluates the rules for one name: which members may serve it.
func (a *API) testRouting(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repo, err := a.routingRepo(ctx, chi.URLParam(r, "name"))
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		a.writeErr(w, r, err)
		return
	}
	e, err := routing.Explain(ctx, a.Deps.Repos, repo, in.Name)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, e)
}

// routingSuggestion is one entry of schema RoutingSuggestions.
type routingSuggestion struct {
	routing.Suggestion
	// Source is the hosted repository publishing the names.
	Source string `json:"source"`
	// Member is, for a group, the direct member the source is reached
	// through: the member the pattern should be exclusive to.
	Member string `json:"member,omitempty"`
}

// suggestRouting proposes patterns from the names hosted repositories
// publish: for a group its hosted members (exclusive to them), for a proxy
// every hosted repository of the format (to block on the proxy). Only
// repositories the caller may read are looked at.
func (a *API) suggestRouting(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := chi.URLParam(r, "name")
	if err := a.authorizeRepoAdmin(ctx, name, "repository.routing.suggest"); err != nil {
		a.writeErr(w, r, err)
		return
	}
	repo, err := a.routingRepo(ctx, name)
	if err != nil {
		a.writeErr(w, r, err)
		return
	}
	type source struct{ repo, member *core.Repository }
	var sources []source
	switch repo.Type {
	case core.RepoGroup:
		members, err := a.Deps.Repos.Members(ctx, repo)
		if err != nil {
			a.writeErr(w, r, err)
			return
		}
		for _, m := range members {
			hosted, err := a.hostedBelow(ctx, m, 0)
			if err != nil {
				a.writeErr(w, r, err)
				return
			}
			for _, h := range hosted {
				sources = append(sources, source{h, m})
			}
		}
	case core.RepoProxy:
		page, err := a.Deps.Repos.List(ctx, core.RepositoryQuery{Format: repo.Format, Type: core.RepoHosted, Limit: core.MaxPageSize})
		if err != nil {
			a.writeErr(w, r, err)
			return
		}
		for _, h := range page.Items {
			sources = append(sources, source{repo: h})
		}
	}
	out := []routingSuggestion{}
	truncated := false
	seen := map[string]bool{}
	for _, s := range sources {
		if seen[s.repo.Name] {
			continue
		}
		seen[s.repo.Name] = true
		if ok, err := a.canRead(ctx, s.repo.Name); err != nil {
			a.writeErr(w, r, err)
			return
		} else if !ok {
			continue
		}
		names, more, err := a.componentNames(ctx, s.repo)
		if err != nil {
			a.writeErr(w, r, err)
			return
		}
		truncated = truncated || more
		for _, sg := range routing.Suggest(repo.Format, names) {
			rs := routingSuggestion{Suggestion: sg, Source: s.repo.Name}
			if s.member != nil {
				rs.Member = s.member.Name
			}
			out = append(out, rs)
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{"repository": repo.Name, "suggestions": out, "truncated": truncated})
}

// hostedBelow returns repo itself when hosted, or a nested group's hosted members.
func (a *API) hostedBelow(ctx context.Context, repo *core.Repository, depth int) ([]*core.Repository, error) {
	switch {
	case repo.Type == core.RepoHosted:
		return []*core.Repository{repo}, nil
	case repo.Type != core.RepoGroup || depth >= 8:
		return nil, nil
	}
	members, err := a.Deps.Repos.Members(ctx, repo)
	if err != nil {
		return nil, err
	}
	var out []*core.Repository
	for _, m := range members {
		hs, err := a.hostedBelow(ctx, m, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, hs...)
	}
	return out, nil
}

// componentNames lists the distinct package identities in repo, up to
// maxSuggestComponents components; more reports that the list was cut.
func (a *API) componentNames(ctx context.Context, repo *core.Repository) (names []string, more bool, err error) {
	q := core.ComponentQuery{RepositoryID: repo.ID, Limit: core.MaxPageSize}
	read := 0
	seen := map[string]bool{}
	for {
		page, err := a.Deps.Components.ListComponents(ctx, q)
		if err != nil {
			return nil, false, err
		}
		for _, c := range page.Items {
			if n := routing.ComponentName(repo.Format, c); !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
		read += len(page.Items)
		if page.NextCursor == "" {
			return names, false, nil
		}
		if read >= maxSuggestComponents {
			return names, true, nil
		}
		q.Cursor = page.NextCursor
	}
}
