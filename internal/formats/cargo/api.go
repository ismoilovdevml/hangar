package cargo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// crateParams reads {crate} and {version} from the route. The crate name is
// returned lowercased.
func crateParams(r *http.Request) (name, version string, err error) {
	n, err1 := url.PathUnescape(chi.URLParam(r, "crate"))
	v, err2 := url.PathUnescape(chi.URLParam(r, "version"))
	if err1 != nil || err2 != nil || !ValidCrateName(n) || (v != "" && !ValidVersion(v)) {
		return "", "", fmt.Errorf("no such crate version: %w", core.ErrNotFound)
	}
	return strings.ToLower(n), v, nil
}

// yank implements DELETE api/v1/crates/{crate}/{version}/yank.
func (h *handler) yank(w http.ResponseWriter, r *http.Request) { h.setYanked(w, r, true) }

// unyank implements PUT api/v1/crates/{crate}/{version}/unyank.
func (h *handler) unyank(w http.ResponseWriter, r *http.Request) { h.setYanked(w, r, false) }

func (h *handler) setYanked(w http.ResponseWriter, r *http.Request, yanked bool) {
	ctx, target, ok := h.authorizeWrite(w, r)
	if !ok {
		return
	}
	name, version, err := crateParams(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	unlock, err := h.lock(ctx, target, name)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	defer unlock()
	comp, err := h.deps.Components.GetComponent(ctx, target.ID, "", name, version)
	if err != nil {
		h.writeError(w, r, fmt.Errorf("crate `%s@%s`: %w", name, version, err))
		return
	}
	if _, _, err := h.deps.Components.Publish(ctx, target, core.PublishRequest{Component: &core.Component{
		Name: comp.Name, Version: comp.Version,
		Attributes: map[string]any{"yanked": yanked, "index_updated_at": nowString()},
	}}); err != nil {
		h.writeError(w, r, err)
		return
	}
	action := "component.yank"
	if !yanked {
		action = "component.unyank"
	}
	if h.deps.Audit != nil {
		_ = h.deps.Audit.Record(ctx, core.AuditEvent{
			Action: action,
			Target: "component:" + target.Name + "/" + core.Coordinates{Name: comp.Name, Version: comp.Version}.String(),
		})
	}
	h.log.InfoContext(ctx, "cargo "+strings.TrimPrefix(action, "component."), "repo", target.Name, "crate", name, "version", version)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// download implements GET api/v1/crates/{crate}/{version}/download, the
// target of the "dl" URL in config.json.
func (h *handler) download(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	ctx, ok := h.authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	name, version, err := crateParams(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	ctx, tally := proxy.WithCacheTally(ctx)
	defer tally.Record(Name, r.Method)
	asset, owner, err := h.openCrate(ctx, repo, name, version, 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if asset.ComponentID != "" {
		c, err := h.deps.Components.GetComponentByID(ctx, asset.ComponentID)
		if err == nil && c.Quarantined {
			h.writeError(w, r, fmt.Errorf("%s %s: %w", c.Name, c.Version, core.ErrQuarantined))
			return
		}
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, owner)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	rc, err := bs.Get(ctx, asset.BlobDigest)
	if err != nil {
		h.writeError(w, r, fmt.Errorf("blob of %s: %w", asset.Path, err))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", crateContentType)
	w.Header().Set("ETag", `"`+asset.BlobDigest.Hex()+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+"-"+version+`.crate"`)
	http.ServeContent(w, r, name+"-"+version+".crate", asset.CreatedAt, rc)
	if r.Method == http.MethodGet {
		h.deps.Components.RecordDownload(ctx, asset)
	}
}

// openCrate finds the .crate of name@version in repo, fetching it from the
// remote for proxies and from the members, in order, for groups. It returns
// the asset and the repository that stores it.
func (h *handler) openCrate(ctx context.Context, repo *core.Repository, name, version string, depth int) (*core.Asset, *core.Repository, error) {
	switch repo.Type {
	case core.RepoGroup:
		var (
			found   *core.Asset
			owner   *core.Repository
			lastErr error
		)
		err := h.eachMember(ctx, repo, depth, name, func(m *core.Repository) error {
			if found != nil {
				return nil
			}
			a, o, err := h.openCrate(ctx, m, name, version, depth+1)
			switch {
			case err == nil:
				found, owner = a, o
			case errors.Is(err, core.ErrNotFound):
			default:
				h.log.WarnContext(ctx, "group member crate lookup failed", "group", repo.Name, "member", m.Name, "crate", name, "err", err)
				lastErr = err
			}
			return nil
		})
		switch {
		case err != nil:
			return nil, nil, err
		case found != nil:
			return found, owner, nil
		case lastErr != nil:
			return nil, nil, lastErr
		}
		return nil, nil, fmt.Errorf("crate `%s@%s`: %w", name, version, core.ErrNotFound)
	case core.RepoProxy:
		if err := routing.Check(ctx, repo, name); err != nil {
			return nil, nil, err
		}
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, cratePath(name, version))
		if err == nil {
			proxy.NoteCacheHit(ctx, a.Size)
			return a, repo, nil
		}
		if !errors.Is(err, core.ErrNotFound) {
			return nil, nil, err
		}
		a, err = h.cacheCrate(ctx, repo, name, version)
		proxy.NoteCacheMiss(ctx, err)
		if err != nil {
			return nil, nil, err
		}
		return a, repo, nil
	default:
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, cratePath(name, version))
		if err != nil {
			return nil, nil, fmt.Errorf("crate `%s@%s`: %w", name, version, err)
		}
		return a, repo, nil
	}
}

// searchHit is one crate in a search answer.
type searchHit struct {
	Name        string `json:"name"`
	MaxVersion  string `json:"max_version"`
	Description string `json:"description"`
}

const (
	defaultPerPage = 10
	maxPerPage     = 100
	// maxSearchNames bounds how many crate names one search collects per repository.
	maxSearchNames = 1000
)

// search implements GET api/v1/crates?q=&per_page=: crates whose name or
// description contains every word of q. Proxy repositories search what
// they have cached.
func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	ctx, ok := h.authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	perPage := defaultPerPage
	if s := r.URL.Query().Get("per_page"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			h.writeError(w, r, fmt.Errorf("per_page must be a positive number: %w", core.ErrInvalid))
			return
		}
		perPage = min(n, maxPerPage)
	}
	hits, err := h.searchRepo(ctx, repo, q, 0)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	sort.Slice(hits, func(i, j int) bool {
		// exact name matches first, then by name
		ei, ej := strings.EqualFold(hits[i].Name, q), strings.EqualFold(hits[j].Name, q)
		if ei != ej {
			return ei
		}
		return strings.ToLower(hits[i].Name) < strings.ToLower(hits[j].Name)
	})
	total := len(hits)
	if len(hits) > perPage {
		hits = hits[:perPage]
	}
	writeJSON(w, http.StatusOK, map[string]any{"crates": hits, "meta": map[string]int{"total": total}})
}

func (h *handler) searchRepo(ctx context.Context, repo *core.Repository, q string, depth int) ([]searchHit, error) {
	if repo.Type == core.RepoGroup {
		var out []searchHit
		seen := map[string]bool{}
		err := h.eachMember(ctx, repo, depth, "", func(m *core.Repository) error {
			hits, err := h.searchRepo(ctx, m, q, depth+1)
			if err != nil {
				h.log.WarnContext(ctx, "group member search failed", "group", repo.Name, "member", m.Name, "err", err)
				return nil
			}
			for _, hit := range hits {
				if !routing.Allows(ctx, repo, m, hit.Name) {
					continue
				}
				if k := strings.ToLower(hit.Name); !seen[k] {
					seen[k] = true
					out = append(out, hit)
				}
			}
			return nil
		})
		return out, err
	}
	comps, err := h.searchCandidates(ctx, repo, q)
	if err != nil {
		return nil, err
	}
	best := map[string]*core.Component{}
	var order []string
	for _, c := range comps {
		if c.Namespace != "" || c.Quarantined || !matches(c, q) {
			continue
		}
		if repo.Type == core.RepoProxy && routing.Check(ctx, repo, c.Name) != nil {
			continue // cached before a routing rule excluded it
		}
		cur, ok := best[c.Name]
		if !ok {
			order = append(order, c.Name)
		}
		if !ok || preferVersion(c, cur) {
			best[c.Name] = c
		}
	}
	out := make([]searchHit, 0, len(order))
	for _, n := range order {
		c := best[n]
		name, _ := c.Attributes["display_name"].(string)
		if name == "" {
			name = c.Name
		}
		desc, _ := c.Attributes["description"].(string)
		out = append(out, searchHit{Name: name, MaxVersion: c.Version, Description: desc})
	}
	return out, nil
}

// searchCandidates returns the components a search may match: from the
// full-text index when the component service has one, else every component.
func (h *handler) searchCandidates(ctx context.Context, repo *core.Repository, q string) ([]*core.Component, error) {
	if s, ok := h.deps.Components.(core.ComponentSearcher); ok {
		empty := ""
		comps, _, err := s.SearchComponents(ctx, core.ComponentTextQuery{RepositoryID: repo.ID, Namespace: &empty, Text: q, MaxNames: maxSearchNames})
		return comps, err
	}
	var out []*core.Component
	cursor := ""
	for {
		page, err := h.deps.Components.ListComponents(ctx, core.ComponentQuery{RepositoryID: repo.ID, Limit: core.MaxPageSize, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" || len(out) >= 50*maxSearchNames {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// matches reports whether every word of q occurs in c's name or description.
func matches(c *core.Component, q string) bool {
	desc, _ := c.Attributes["description"].(string)
	hay := strings.ToLower(c.Name + " " + desc)
	for _, word := range strings.Fields(q) {
		if !strings.Contains(hay, word) {
			return false
		}
	}
	return true
}

// preferVersion reports whether c should be shown instead of cur as a
// crate's max_version: releases over yanked versions, then the higher version.
func preferVersion(c, cur *core.Component) bool {
	cy, _ := c.Attributes["yanked"].(bool)
	uy, _ := cur.Attributes["yanked"].(bool)
	if cy != uy {
		return !cy
	}
	return compareVersions(c.Version, cur.Version) > 0
}

// listOwners implements GET api/v1/crates/{crate}/owners. Hangar has no
// per-crate owners: anyone with repo:<name>:write may publish and yank. The
// answer lists the principals that published the crate's versions.
func (h *handler) listOwners(w http.ResponseWriter, r *http.Request) {
	repo := core.RepositoryFrom(r.Context())
	ctx, ok := h.authorize(w, r, repo, core.ActionRead)
	if !ok {
		return
	}
	name, _, err := crateParams(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	target := repo
	if repo.Type == core.RepoGroup {
		if t, err := h.writeTarget(ctx, repo); err == nil && t != nil {
			target = t
		}
	}
	comps, err := h.crateComponents(ctx, target.ID, name)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if len(comps) == 0 {
		h.writeError(w, r, fmt.Errorf("crate %q: %w", name, core.ErrNotFound))
		return
	}
	type user struct {
		ID    int    `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	users := []user{}
	seen := map[string]bool{}
	for _, c := range comps {
		if c.PublishedBy == "" || seen[c.PublishedBy] {
			continue
		}
		seen[c.PublishedBy] = true
		users = append(users, user{ID: len(users) + 1, Login: c.PublishedBy, Name: c.PublishedBy})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

// changeOwners answers cargo owner --add / --remove: ownership follows the
// repository's permissions, so it is changed in Hangar, not through cargo.
func (h *handler) changeOwners(w http.ResponseWriter, r *http.Request) {
	_, target, ok := h.authorizeWrite(w, r)
	if !ok {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	h.writeError(w, r, fmt.Errorf("crate owners are not managed through cargo: grant repo:%s:write to a user or service account in Hangar instead: %w",
		target.Name, core.ErrNotImplemented))
}
