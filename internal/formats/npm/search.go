package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// npm search (GET /-/v1/search?text=&size=&from=) limits, as on the public
// registry.
const (
	defaultSearchSize = 20
	maxSearchSize     = 250
	maxSearchFrom     = 5000
	// maxSearchScan bounds the components a hosted search reads when the
	// component service has no full-text index.
	maxSearchScan = 20000
)

// searchHit is one result: the package name and its search object.
type searchHit struct {
	name string
	obj  json.RawMessage
}

// search answers npm search: hosted repositories from Hangar's component
// search, proxies from their remote's search endpoint (results the routing
// rules exclude are dropped), groups by merging their members' results in
// member order (the first member listing a name wins).
func (h *handler) search(w http.ResponseWriter, r *http.Request, repo *core.Repository) error {
	if !isRead(r.Method) {
		return errMethod
	}
	ctx := r.Context()
	if err := h.authorize(ctx, core.ActionRead, repo); err != nil {
		return err
	}
	q := r.URL.Query()
	text := strings.TrimSpace(q.Get("text"))
	size, err := queryInt(q, "size", defaultSearchSize, 1, maxSearchSize)
	if err != nil {
		return err
	}
	from, err := queryInt(q, "from", 0, 0, maxSearchFrom)
	if err != nil {
		return err
	}
	hits, total, err := h.searchRepo(ctx, repo, text, from+size, 0)
	if err != nil {
		return err
	}
	page := []json.RawMessage{}
	for i := from; i < len(hits) && i < from+size; i++ {
		page = append(page, hits[i].obj)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"objects": page, "total": max(total, len(hits)), "time": h.f.now().UTC().Format(time.RFC1123),
	})
	return nil
}

func queryInt(q url.Values, key string, def, lo, hi int) (int, error) {
	s := q.Get(key)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo {
		return 0, fmt.Errorf("%s=%q: want a number of at least %d: %w", key, s, lo, core.ErrInvalid)
	}
	return min(n, hi), nil
}

// searchRepo returns up to limit results of repo, best first, and the
// number of matches it knows of.
func (h *handler) searchRepo(ctx context.Context, repo *core.Repository, text string, limit, depth int) ([]searchHit, int, error) {
	switch repo.Type {
	case core.RepoHosted:
		return h.searchHosted(ctx, repo, text, limit)
	case core.RepoProxy:
		return h.searchProxy(ctx, repo, text, limit)
	case core.RepoGroup:
		return h.searchGroup(ctx, repo, text, limit, depth)
	}
	return nil, 0, fmt.Errorf("repository type %q: %w", repo.Type, core.ErrInvalid)
}

func (h *handler) searchGroup(ctx context.Context, group *core.Repository, text string, limit, depth int) ([]searchHit, int, error) {
	if depth >= maxGroupDepth {
		return nil, 0, fmt.Errorf("group %q: nesting deeper than %d: %w", group.Name, maxGroupDepth, core.ErrInvalid)
	}
	members, err := h.deps.Repos.Members(ctx, group)
	if err != nil {
		return nil, 0, err
	}
	var out []searchHit
	seen := map[string]bool{}
	total, answered := 0, 0
	var lastErr error
	for _, m := range members {
		if m.Format != Name || !m.Online {
			continue
		}
		hits, n, err := h.searchRepo(ctx, m, text, limit, depth+1)
		if err != nil {
			// Search is best effort across members: one failing remote
			// does not hide the others' results.
			h.deps.Logger.WarnContext(ctx, "npm search: group member failed", "group", group.Name, "member", m.Name, "error", err)
			lastErr = err
			continue
		}
		answered++
		total += n
		for _, hit := range hits {
			if seen[hit.name] || !routing.Allows(ctx, group, m, hit.name) {
				continue
			}
			seen[hit.name] = true
			out = append(out, hit)
		}
	}
	if answered == 0 && lastErr != nil {
		return nil, 0, lastErr
	}
	return out, total, nil
}

func (h *handler) searchProxy(ctx context.Context, repo *core.Repository, text string, limit int) ([]searchHit, int, error) {
	up, err := h.deps.Upstreams.ForRepository(ctx, repo)
	if err != nil {
		return nil, 0, err
	}
	q := url.Values{"text": {text}, "size": {strconv.Itoa(min(limit, maxSearchSize))}, "from": {"0"}}
	resp, err := up.Fetch(ctx, core.UpstreamRequest{Path: "-/v1/search?" + q.Encode(), Index: true,
		Header: http.Header{"Accept": {mediaJSON}}})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("remote search answered %d: %w", resp.StatusCode, core.ErrUpstream)
	}
	var doc struct {
		Objects []json.RawMessage `json:"objects"`
		Total   int               `json:"total"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&doc); err != nil {
		return nil, 0, fmt.Errorf("remote search answer is not valid JSON: %w", core.ErrUpstream)
	}
	out := make([]searchHit, 0, len(doc.Objects))
	for _, o := range doc.Objects {
		var head struct {
			Package struct {
				Name string `json:"name"`
			} `json:"package"`
		}
		if json.Unmarshal(o, &head) != nil || head.Package.Name == "" {
			continue
		}
		if routing.Check(ctx, repo, head.Package.Name) != nil {
			continue
		}
		out = append(out, searchHit{name: head.Package.Name, obj: o})
	}
	return out, doc.Total, nil
}

// searchHosted matches the repository's components and describes each
// package by its highest version that is not quarantined.
func (h *handler) searchHosted(ctx context.Context, repo *core.Repository, text string, limit int) ([]searchHit, int, error) {
	comps, err := h.searchCandidates(ctx, repo, text, limit)
	if err != nil {
		return nil, 0, err
	}
	type pkg struct {
		name pkgName
		best *core.Component
	}
	byName := map[string]*pkg{}
	var order []string
	for _, c := range comps {
		if c.Quarantined {
			continue
		}
		full := c.Name
		if c.Namespace != "" {
			full = c.Namespace + "/" + c.Name
		}
		n, err := parseName(full)
		if err != nil {
			continue
		}
		p := byName[full]
		if p == nil {
			p = &pkg{name: n}
			byName[full] = p
			order = append(order, full)
		}
		if p.best == nil || highestVersion([]string{p.best.Version, c.Version}) == c.Version {
			p.best = c
		}
	}
	// Exact name matches first, then by name.
	lower := strings.ToLower(text)
	slices.SortFunc(order, func(a, b string) int {
		ea, eb := strings.ToLower(a) == lower || strings.ToLower(byName[a].name.Base) == lower,
			strings.ToLower(b) == lower || strings.ToLower(byName[b].name.Base) == lower
		if ea != eb {
			if ea {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	total := len(order)
	if len(order) > limit {
		order = order[:limit]
	}
	bs, err := h.deps.Blobs.ForRepository(ctx, repo)
	if err != nil {
		return nil, 0, err
	}
	out := make([]searchHit, 0, len(order))
	for _, full := range order {
		p := byName[full]
		obj, err := json.Marshal(h.searchObject(ctx, repo, bs, p.name, p.best))
		if err != nil {
			return nil, 0, err
		}
		out = append(out, searchHit{name: full, obj: obj})
	}
	return out, total, nil
}

// searchCandidates returns the components a search may match: from the
// full-text index when the component service has one, else from a bounded
// listing filtered by name and description.
func (h *handler) searchCandidates(ctx context.Context, repo *core.Repository, text string, limit int) ([]*core.Component, error) {
	if s, ok := h.deps.Components.(core.ComponentSearcher); ok {
		comps, _, err := s.SearchComponents(ctx, core.ComponentTextQuery{RepositoryID: repo.ID, Text: text, MaxNames: limit})
		return comps, err
	}
	words := strings.Fields(strings.ToLower(text))
	var out []*core.Component
	q := core.ComponentQuery{RepositoryID: repo.ID, Limit: core.MaxPageSize}
	for scanned := 0; scanned < maxSearchScan; {
		page, err := h.deps.Components.ListComponents(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, c := range page.Items {
			desc, _ := c.Attributes["description"].(string)
			hay := strings.ToLower(c.Namespace + "/" + c.Name + " " + desc)
			if !slices.ContainsFunc(words, func(w string) bool {
				_, v, ok := strings.Cut(w, ":")
				if ok {
					w = v
				}
				return !strings.Contains(hay, w)
			}) {
				out = append(out, c)
			}
		}
		scanned += len(page.Items)
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
	}
	return out, nil
}

// searchObject is the result object npm prints for one hosted package.
func (h *handler) searchObject(ctx context.Context, repo *core.Repository, bs core.BlobStore, n pkgName, c *core.Component) map[string]any {
	desc, _ := c.Attributes["description"].(string)
	keywords := []string{}
	if m, err := h.readManifest(ctx, repo, bs, n, c.Version); err == nil {
		if desc == "" {
			desc = m.str("description")
		}
		if raw, ok := m["keywords"]; ok {
			var kw []string
			if json.Unmarshal(raw, &kw) == nil {
				keywords = kw
			}
		}
	} else if !errors.Is(err, core.ErrNotFound) {
		h.deps.Logger.WarnContext(ctx, "npm search: version manifest unreadable", "repository", repo.Name,
			"package", n.Full, "version", c.Version, "error", err)
	}
	scope := "unscoped"
	if n.Scope != "" {
		scope = strings.TrimPrefix(n.Scope, "@")
	}
	people := []map[string]string{}
	publisher := map[string]string{}
	if c.PublishedBy != "" {
		publisher["username"] = c.PublishedBy
		people = append(people, map[string]string{"username": c.PublishedBy})
	}
	return map[string]any{
		"package": map[string]any{
			"name": n.Full, "scope": scope, "version": c.Version, "description": desc, "keywords": keywords,
			"date":      c.CreatedAt.UTC().Format(timeFormat),
			"links":     map[string]string{"npm": h.tarballBase(repo) + "/" + n.Escaped()},
			"publisher": publisher, "maintainers": people,
		},
		"flags":       map[string]any{},
		"score":       map[string]any{"final": 1, "detail": map[string]float64{"quality": 1, "popularity": 1, "maintenance": 1}},
		"searchScore": 1,
	}
}
