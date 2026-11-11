package nuget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/quarantine"
	"github.com/ismoilovdevml/hangar/internal/proxy"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// maxGroupDepth bounds group nesting (and breaks accidental cycles).
const maxGroupDepth = 8

// entry is one package version as a registration source sees it. catalog is
// the registration catalog entry without any URL properties; they are added
// when rendering for the repository the client addressed.
type entry struct {
	ver       Version
	catalog   map[string]any
	listed    bool
	semver2   bool
	published time.Time
	downloads int64
}

// entries returns every version of package lid in repo, sorted ascending.
// ErrNotFound when the repository has none.
func (h *handler) entries(ctx context.Context, repo *core.Repository, lid string, depth int) ([]entry, error) {
	var out []entry
	switch repo.Type {
	case core.RepoHosted:
		comps, err := h.components(ctx, repo, lid)
		if err != nil {
			return nil, err
		}
		for _, c := range comps {
			if e, ok := componentEntry(c); ok {
				out = append(out, e)
			}
		}
	case core.RepoProxy:
		if err := routing.Check(ctx, repo, lid); err != nil {
			return nil, err
		}
		cats, err := h.proxyRegistration(ctx, repo, lid)
		if err != nil {
			return nil, err
		}
		blocked, err := h.blockedVersions(ctx, repo, lid)
		if err != nil {
			return nil, err
		}
		for _, c := range cats {
			if e, ok := catalogEntry(c); ok && !blocked[e.ver.Lower()] {
				out = append(out, e)
			}
		}
	case core.RepoGroup:
		seen := map[string]bool{}
		err := h.eachMember(ctx, repo, depth, lid, func(m *core.Repository) error {
			es, err := h.entries(ctx, m, lid, depth+1)
			for _, e := range es {
				if k := e.ver.Lower(); !seen[k] {
					seen[k] = true
					out = append(out, e)
				}
			}
			return err
		})
		if err != nil && len(out) == 0 {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("package %q: %w", lid, core.ErrNotFound)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ver.Compare(out[j].ver) < 0 })
	return out, nil
}

// versions returns the lowercased normalized versions of lid available for
// download from repo (listed and unlisted), deduplicated and sorted.
func (h *handler) versions(ctx context.Context, repo *core.Repository, lid string, depth int) ([]Version, error) {
	var out []Version
	switch repo.Type {
	case core.RepoHosted:
		comps, err := h.components(ctx, repo, lid)
		if err != nil {
			return nil, err
		}
		for _, c := range comps {
			if v, err := ParseVersion(c.Version); err == nil {
				out = append(out, v)
			}
		}
	case core.RepoProxy:
		if err := routing.Check(ctx, repo, lid); err != nil {
			return nil, err
		}
		vs, err := h.proxyVersions(ctx, repo, lid)
		if err != nil {
			return nil, err
		}
		blocked, err := h.blockedVersions(ctx, repo, lid)
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			if !blocked[v.Lower()] {
				out = append(out, v)
			}
		}
	case core.RepoGroup:
		err := h.eachMember(ctx, repo, depth, lid, func(m *core.Repository) error {
			vs, err := h.versions(ctx, m, lid, depth+1)
			out = append(out, vs...)
			return err
		})
		if err != nil && len(out) == 0 {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("package %q: %w", lid, core.ErrNotFound)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Compare(out[j]) < 0 })
	dedup := out[:1]
	for _, v := range out[1:] {
		if v.Lower() != dedup[len(dedup)-1].Lower() {
			dedup = append(dedup, v)
		}
	}
	return dedup, nil
}

// locate finds the asset at path (a flat container file of lid/lver) in repo,
// caching it from upstream in proxies. It returns the repository that holds it.
func (h *handler) locate(ctx context.Context, repo *core.Repository, lid, lver, path string, depth int) (*core.Repository, *core.Asset, error) {
	switch repo.Type {
	case core.RepoHosted:
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, path)
		return repo, a, err
	case core.RepoProxy:
		if err := routing.Check(ctx, repo, lid); err != nil {
			return nil, nil, err
		}
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, path)
		if err == nil {
			proxy.NoteCacheHit(ctx, a.Size)
		}
		if err == nil || !errors.Is(err, core.ErrNotFound) {
			return repo, a, err
		}
		uncached, err := h.cachePackage(ctx, repo, lid, lver)
		proxy.NoteCacheMiss(ctx, err)
		if err != nil {
			return nil, nil, err
		}
		for _, u := range uncached {
			if u.Path == path {
				return repo, u, nil
			}
		}
		a, err = h.deps.Components.GetAsset(ctx, repo.ID, path)
		return repo, a, err
	case core.RepoGroup:
		var (
			found *core.Repository
			asset *core.Asset
		)
		err := h.eachMember(ctx, repo, depth, lid, func(m *core.Repository) error {
			if found != nil {
				return nil
			}
			r, a, err := h.locate(ctx, m, lid, lver, path, depth+1)
			if err == nil {
				found, asset = r, a
			}
			return err
		})
		if found != nil {
			return found, asset, nil
		}
		if err == nil {
			err = fmt.Errorf("%s: %w", path, core.ErrNotFound)
		}
		return nil, nil, err
	}
	return nil, nil, fmt.Errorf("repository type %q: %w", repo.Type, core.ErrInvalid)
}

// eachMember calls fn for every member of group in order that the group's
// routing rules let serve package lid ("" for every member: callers that
// merge many packages filter with routing.Allows). Not-found results are
// ignored; the first other error is returned after all members ran, so one
// unreachable proxy does not hide the others. When the rules exclude every
// member the error is the *core.RoutingBlock (a 404 naming the rule).
func (h *handler) eachMember(ctx context.Context, group *core.Repository, depth int, lid string, fn func(*core.Repository) error) error {
	if depth >= maxGroupDepth {
		return fmt.Errorf("group %q nested too deeply: %w", group.Name, core.ErrInvalid)
	}
	var (
		members []*core.Repository
		err     error
	)
	if lid == "" {
		members, err = h.deps.Repos.Members(ctx, group)
	} else {
		members, err = routing.Members(ctx, h.deps.Repos, group, lid)
	}
	if err != nil {
		return err
	}
	var first error
	for _, m := range members {
		if m.Format != Name || !m.Online {
			continue
		}
		if err := fn(m); err != nil && !errors.Is(err, core.ErrNotFound) {
			h.log().WarnContext(ctx, "nuget group member failed", "group", group.Name, "member", m.Name, "err", err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// components lists the versions of lid in a hosted (or proxy cache)
// repository, without quarantined ones: a version a client cannot download
// is not advertised either.
func (h *handler) components(ctx context.Context, repo *core.Repository, lid string) ([]*core.Component, error) {
	comps, err := h.listComponents(ctx, core.ComponentQuery{RepositoryID: repo.ID, Name: lid})
	return quarantine.Drop(comps), err
}

// blockedVersions returns the quarantined versions of lid cached in a proxy
// repository; they are left out of the upstream's lists.
func (h *handler) blockedVersions(ctx context.Context, repo *core.Repository, lid string) (map[string]bool, error) {
	empty := ""
	return quarantine.Versions(ctx, h.deps.Components, repo.ID, &empty, lid)
}

func (h *handler) listComponents(ctx context.Context, q core.ComponentQuery) ([]*core.Component, error) {
	empty := ""
	q.Namespace = &empty
	q.Limit = core.MaxPageSize
	var out []*core.Component
	for {
		page, err := h.deps.Components.ListComponents(ctx, q)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		q.Cursor = page.NextCursor
	}
}

// componentEntry converts a stored component into a registration entry.
func componentEntry(c *core.Component) (entry, bool) {
	md, err := componentMetadata(c)
	if err != nil {
		return entry{}, false
	}
	v, err := ParseVersion(md.Version)
	if err != nil {
		return entry{}, false
	}
	listed := true
	if b, ok := c.Attributes[attrListed].(bool); ok {
		listed = b
	}
	return entry{
		ver: v, catalog: md.catalog(), listed: listed, semver2: md.IsSemVer2(),
		published: c.CreatedAt, downloads: c.DownloadCount,
	}, true
}

// componentMetadata decodes the manifest metadata stored on a component.
func componentMetadata(c *core.Component) (*Metadata, error) {
	raw, ok := c.Attributes[attrMeta]
	if !ok {
		return nil, fmt.Errorf("component %s has no nuget metadata: %w", c.ID, core.ErrInvalid)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}
	var md Metadata
	if err := json.Unmarshal(b, &md); err != nil {
		return nil, fmt.Errorf("decode metadata: %w", err)
	}
	if md.ID == "" || md.Version == "" {
		return nil, fmt.Errorf("component %s: incomplete metadata: %w", c.ID, core.ErrInvalid)
	}
	return &md, nil
}

// catalogEntry converts an upstream registration catalog entry.
func catalogEntry(c map[string]any) (entry, bool) {
	vs, _ := c["version"].(string)
	v, err := ParseVersion(vs)
	if err != nil {
		return entry{}, false
	}
	e := entry{ver: v, catalog: c, listed: true}
	if b, ok := c["listed"].(bool); ok {
		e.listed = b
	}
	if p, ok := c["published"].(string); ok {
		if t, err := time.Parse(time.RFC3339, p); err == nil {
			e.published = t
			if t.Year() == 1900 {
				e.listed = false // legacy unlisted marker
			}
		}
	}
	e.semver2 = v.IsSemVer2() || catalogDepsSemVer2(c)
	return e, true
}

func catalogDepsSemVer2(c map[string]any) bool {
	groups, _ := c["dependencyGroups"].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		deps, _ := gm["dependencies"].([]any)
		for _, d := range deps {
			dm, _ := d.(map[string]any)
			if r, ok := dm["range"].(string); ok && rangeIsSemVer2(r) {
				return true
			}
		}
	}
	return false
}

// lowerID lowercases a package id the way clients build URLs.
func lowerID(id string) string { return strings.ToLower(id) }
