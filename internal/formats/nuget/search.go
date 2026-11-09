package nuget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats/internal/quarantine"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// Search limits (nuget.org uses the same).
const (
	defaultTake = 20
	maxTake     = 1000
	maxSkip     = 3000
)

// maxSearchPackages bounds the packages a text search of a hosted repository
// considers. Matches are ordered by id, so a query matching more packages
// than this loses the tail (and totalHits is capped accordingly).
const maxSearchPackages = 10000

type searchQuery struct {
	q           string
	skip, take  int
	prerelease  bool
	semver2     bool
	semVerLevel string
	packageType string
}

func parseSearch(r *http.Request) searchQuery {
	v := r.URL.Query()
	q := searchQuery{
		q:           strings.TrimSpace(v.Get("q")),
		take:        defaultTake,
		semVerLevel: v.Get("semVerLevel"),
		packageType: strings.TrimSpace(v.Get("packageType")),
	}
	if n, err := strconv.Atoi(v.Get("skip")); err == nil && n > 0 {
		q.skip = min(n, maxSkip)
	}
	if n, err := strconv.Atoi(v.Get("take")); err == nil && n >= 0 {
		q.take = min(n, maxTake)
	}
	q.prerelease, _ = strconv.ParseBool(v.Get("prerelease"))
	if lv, err := ParseVersion(q.semVerLevel); err == nil && lv.Major >= 2 {
		q.semver2 = true
	}
	return q
}

// hit is one search result: a package id and its result object. The
// object's URL properties are set when rendering.
type hit struct {
	lid string
	doc map[string]any
}

// search handles GET query.
func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	repo, r, ok := h.readRequest(w, r)
	if !ok {
		return
	}
	q := parseSearch(r)
	total, hits, err := h.searchRepo(r.Context(), repo, q, 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	end := min(q.skip+q.take, len(hits))
	start := min(q.skip, end)
	if repo.Type == core.RepoProxy {
		start = 0 // the upstream already skipped
		end = min(q.take, len(hits))
	}
	hive := hiveSemVer1
	if q.semver2 {
		hive = hiveGzSemVer2
	}
	data := make([]any, 0, end-start)
	for _, x := range hits[start:end] {
		data = append(data, renderHit(h.repoURL(repo), hive, x))
	}
	h.writeJSON(w, r, map[string]any{"totalHits": total, "data": data}, false)
}

// searchRepo returns the total number of matches and the matches up to
// skip+take (all of them for hosted repositories).
func (h *handler) searchRepo(ctx context.Context, repo *core.Repository, q searchQuery, depth int) (int, []hit, error) {
	switch repo.Type {
	case core.RepoHosted:
		hits, err := h.searchHosted(ctx, repo, q)
		return len(hits), hits, err
	case core.RepoProxy:
		return h.searchProxy(ctx, repo, q)
	case core.RepoGroup:
		sub := q
		sub.take, sub.skip = min(q.skip+q.take, maxTake), 0
		var (
			total int
			hits  []hit
			index = map[string]int{}
		)
		err := h.eachMember(ctx, repo, depth, "", func(m *core.Repository) error {
			n, hs, err := h.searchRepo(ctx, m, sub, depth+1)
			total += n
			for _, x := range hs {
				if !routing.Allows(ctx, repo, m, x.lid) {
					total--
					continue
				}
				if i, ok := index[x.lid]; ok {
					hits[i].doc = mergeHits(hits[i].doc, x.doc)
					total--
					continue
				}
				index[x.lid] = len(hits)
				hits = append(hits, x)
			}
			return err
		})
		if err != nil && len(hits) == 0 {
			return 0, nil, err
		}
		return total, hits, nil
	}
	return 0, nil, fmt.Errorf("repository type %q: %w", repo.Type, core.ErrInvalid)
}

func (h *handler) searchHosted(ctx context.Context, repo *core.Repository, q searchQuery) ([]hit, error) {
	comps, err := h.searchCandidates(ctx, repo, q)
	if err != nil {
		return nil, err
	}
	byID := map[string][]entry{}
	for _, c := range comps {
		e, ok := componentEntry(c)
		if !ok || !e.listed || (e.ver.IsPrerelease() && !q.prerelease) || (e.semver2 && !q.semver2) {
			continue
		}
		byID[c.Name] = append(byID[c.Name], e)
	}
	terms := strings.Fields(strings.ToLower(q.q))
	var hits []hit
	for lid, es := range byID {
		sort.Slice(es, func(i, j int) bool { return es[i].ver.Compare(es[j].ver) < 0 })
		latest := es[len(es)-1]
		if !matchTerms(lid, latest.catalog, terms) || !matchPackageType(latest.catalog, q.packageType) {
			continue
		}
		hits = append(hits, hit{lid: lid, doc: hostedHit(es)})
	}
	exact := ""
	if len(terms) == 1 {
		exact = terms[0]
	}
	sort.Slice(hits, func(i, j int) bool {
		if (hits[i].lid == exact) != (hits[j].lid == exact) {
			return hits[i].lid == exact
		}
		return hits[i].lid < hits[j].lid
	})
	return hits, nil
}

// searchCandidates returns the components a query can match. With a text
// query and a component service that keeps a full-text index, only the
// packages whose stored text contains every query token (as a word prefix)
// are loaded; matchTerms then applies the exact NuGet field rules to them.
// Without either, every component of the repository is a candidate.
func (h *handler) searchCandidates(ctx context.Context, repo *core.Repository, q searchQuery) ([]*core.Component, error) {
	searcher, ok := h.deps.Components.(core.ComponentSearcher)
	if !ok || strings.TrimSpace(q.q) == "" {
		comps, err := h.listComponents(ctx, core.ComponentQuery{RepositoryID: repo.ID})
		return quarantine.Drop(comps), err
	}
	empty := ""
	comps, truncated, err := searcher.SearchComponents(ctx, core.ComponentTextQuery{
		RepositoryID: repo.ID, Namespace: &empty, Text: q.q, MaxNames: maxSearchPackages,
	})
	if err != nil {
		return nil, err
	}
	if truncated {
		h.log().WarnContext(ctx, "nuget search matched too many packages; results truncated",
			"repository", repo.Name, "query", q.q, "limit", maxSearchPackages)
	}
	return quarantine.Drop(comps), nil
}

// matchTerms reports whether every term matches the package. Terms may be
// field-qualified: PackageId:<exact id>, id:, tags:/tag:, author:/authors:,
// title:, description:, summary:. Unqualified terms match the id, title,
// description, summary, tags and authors.
func matchTerms(lid string, cat map[string]any, terms []string) bool {
	str := func(k string) string { s, _ := cat[k].(string); return strings.ToLower(s) }
	tags := strings.ToLower(joinAny(cat["tags"]))
	for _, t := range terms {
		field, val, ok := strings.Cut(t, ":")
		if !ok {
			field, val = "", t
		}
		val = strings.Trim(val, `"`)
		var m bool
		switch field {
		case "packageid": //nolint:misspell // NuGet search field name
			m = lid == val
		case "id":
			m = strings.Contains(lid, val)
		case "tags", "tag":
			m = strings.Contains(tags, val)
		case "author", "authors":
			m = strings.Contains(str("authors"), val)
		case "title", "description", "summary":
			m = strings.Contains(str(field), val)
		default:
			m = strings.Contains(lid, t) || strings.Contains(str("title"), t) || strings.Contains(str("description"), t) ||
				strings.Contains(str("summary"), t) || strings.Contains(tags, t) || strings.Contains(str("authors"), t)
		}
		if !m {
			return false
		}
	}
	return true
}

func matchPackageType(cat map[string]any, want string) bool {
	if want == "" {
		return true
	}
	types, _ := cat["packageTypes"].([]any)
	if len(types) == 0 {
		return strings.EqualFold(want, "Dependency")
	}
	for _, t := range types {
		if tm, ok := t.(map[string]any); ok {
			if n, _ := tm["name"].(string); strings.EqualFold(n, want) {
				return true
			}
		}
	}
	return false
}

func joinAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	case []string:
		return strings.Join(t, " ")
	}
	return ""
}

// hostedHit builds a search result from the visible versions (ascending).
func hostedHit(es []entry) map[string]any {
	latest := es[len(es)-1]
	c := latest.catalog
	versions := make([]any, len(es))
	var downloads int64
	for i, e := range es {
		versions[i] = map[string]any{"version": e.ver.Full(), "downloads": e.downloads}
		downloads += e.downloads
	}
	doc := map[string]any{
		"@type":           "Package",
		"id":              c["id"],
		"version":         latest.ver.Full(),
		"description":     c["description"],
		"summary":         c["summary"],
		"title":           c["title"],
		"iconUrl":         c["iconUrl"],
		"licenseUrl":      c["licenseUrl"],
		"projectUrl":      c["projectUrl"],
		"tags":            c["tags"],
		"authors":         splitAuthors(c["authors"]),
		"owners":          c["owners"],
		"totalDownloads":  downloads,
		"verified":        false,
		"versions":        versions,
		"vulnerabilities": []any{},
		"packageTypes":    []any{map[string]any{"name": "Dependency"}},
	}
	if types, ok := c["packageTypes"].([]any); ok && len(types) > 0 {
		doc["packageTypes"] = types
	}
	for k, v := range doc {
		if v == nil {
			delete(doc, k)
		}
	}
	return doc
}

func splitAuthors(v any) []string {
	s, _ := v.(string)
	var out []string
	for _, a := range strings.Split(s, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// renderHit sets the URL properties of a search result for the addressed repository.
func renderHit(base, hive string, x hit) map[string]any {
	doc := make(map[string]any, len(x.doc)+2)
	for k, v := range x.doc {
		doc[k] = v
	}
	reg := base + "v3/" + hive + "/" + x.lid + "/"
	doc["@id"] = reg + "index.json"
	doc["registration"] = reg + "index.json"
	if vs, ok := doc["versions"].([]any); ok {
		out := make([]any, 0, len(vs))
		for _, v := range vs {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			s, _ := vm["version"].(string)
			pv, err := ParseVersion(s)
			if err != nil {
				continue
			}
			n := map[string]any{"version": pv.Full(), "downloads": vm["downloads"], "@id": reg + pv.Lower() + ".json"}
			if n["downloads"] == nil {
				n["downloads"] = 0
			}
			out = append(out, n)
		}
		doc["versions"] = out
	}
	return doc
}

// mergeHits merges the versions of b into a (same package from two group members).
func mergeHits(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a))
	for k, v := range a {
		out[k] = v
	}
	av, _ := a["versions"].([]any)
	bv, _ := b["versions"].([]any)
	seen := map[string]bool{}
	var all []any
	var best Version
	hasBest := false
	for _, v := range append(append([]any{}, av...), bv...) {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		s, _ := vm["version"].(string)
		pv, err := ParseVersion(s)
		if err != nil || seen[pv.Lower()] {
			continue
		}
		seen[pv.Lower()] = true
		all = append(all, vm)
		if !hasBest || pv.Compare(best) > 0 {
			best, hasBest = pv, true
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		vi, _ := ParseVersion(all[i].(map[string]any)["version"].(string))
		vj, _ := ParseVersion(all[j].(map[string]any)["version"].(string))
		return vi.Compare(vj) < 0
	})
	out["versions"] = all
	if hasBest {
		out["version"] = best.Full()
	}
	return out
}

// searchProxy forwards the query to the upstream search service.
func (h *handler) searchProxy(ctx context.Context, repo *core.Repository, q searchQuery) (int, []hit, error) {
	idx, err := h.upstreamIndex(ctx, repo)
	if err != nil {
		return 0, nil, err
	}
	cachedOnly := func() (int, []hit, error) {
		all, err := h.searchHosted(ctx, repo, q)
		if err != nil {
			return 0, nil, err
		}
		hits := all[:0]
		for _, x := range all {
			if routing.Check(ctx, repo, x.lid) == nil {
				hits = append(hits, x)
			}
		}
		total := len(hits)
		end := min(q.skip+q.take, len(hits))
		return total, hits[min(q.skip, end):end], nil
	}
	if idx.Search == "" || (q.q != "" && repo.RoutingBlock(q.q) != nil) {
		// No search service upstream, or the query is a name the routing
		// rules keep from the remote (it is not sent there): search the
		// packages this proxy has cached.
		return cachedOnly()
	}
	params := url.Values{}
	if q.q != "" {
		params.Set("q", q.q)
	}
	params.Set("skip", strconv.Itoa(q.skip))
	params.Set("take", strconv.Itoa(q.take))
	params.Set("prerelease", strconv.FormatBool(q.prerelease))
	if q.semVerLevel != "" {
		params.Set("semVerLevel", q.semVerLevel)
	}
	if q.packageType != "" {
		params.Set("packageType", q.packageType)
	}
	sep := "?"
	if strings.Contains(idx.Search, "?") {
		sep = "&"
	}
	b, err := h.fetchDoc(ctx, repo, idx.Search+sep+params.Encode(), "", maxSearchResults)
	if errors.Is(err, core.ErrUpstream) {
		// Search host down, rate limited or the repository is offline:
		// answer from what this proxy has cached rather than fail.
		h.log().WarnContext(ctx, "nuget upstream search failed, searching the cache", "repo", repo.Name, "err", err)
		return cachedOnly()
	}
	if err != nil {
		return 0, nil, err
	}
	var res struct {
		TotalHits int              `json:"totalHits"`
		Data      []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return 0, nil, fmt.Errorf("upstream search: %w: %w", err, core.ErrUpstream)
	}
	hits := make([]hit, 0, len(res.Data))
	total := res.TotalHits
	var blocked map[string]map[string]bool // lid -> quarantined versions cached here
	if len(res.Data) > 0 {
		if blocked, err = h.quarantinedByID(ctx, repo); err != nil {
			return 0, nil, err
		}
	}
	for _, d := range res.Data {
		id, _ := d["id"].(string)
		if id == "" {
			continue
		}
		if routing.Check(ctx, repo, id) != nil {
			total--
			continue
		}
		lid := lowerID(id)
		if b := blocked[lid]; len(b) > 0 {
			var ok bool
			if d, ok = dropVersions(d, b); !ok {
				total--
				continue
			}
		}
		hits = append(hits, hit{lid: lid, doc: d})
	}
	return total, hits, nil
}

// quarantinedByID returns the quarantined versions cached in a proxy
// repository, by package id.
func (h *handler) quarantinedByID(ctx context.Context, repo *core.Repository) (map[string]map[string]bool, error) {
	comps, err := quarantine.Components(ctx, h.deps.Components, repo.ID)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]bool{}
	for _, c := range comps {
		if out[c.Name] == nil {
			out[c.Name] = map[string]bool{}
		}
		out[c.Name][c.Version] = true
	}
	return out, nil
}

// dropVersions removes the blocked versions from an upstream search result
// and points "version" at the newest remaining one. It reports false when
// no version remains.
func dropVersions(doc map[string]any, blocked map[string]bool) (map[string]any, bool) {
	vs, _ := doc["versions"].([]any)
	kept := make([]any, 0, len(vs))
	var best Version
	hasBest := false
	for _, v := range vs {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		s, _ := vm["version"].(string)
		pv, err := ParseVersion(s)
		if err != nil || blocked[pv.Lower()] {
			continue
		}
		kept = append(kept, vm)
		if !hasBest || pv.Compare(best) > 0 {
			best, hasBest = pv, true
		}
	}
	if !hasBest {
		return nil, false
	}
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		out[k] = v
	}
	out["versions"] = kept
	out["version"] = best.Full()
	return out, true
}
