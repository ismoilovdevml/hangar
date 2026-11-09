package helm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/routing"
)

// maxGroupDepth bounds group nesting (and breaks accidental cycles).
const maxGroupDepth = 8

// entry is one chart version as index.yaml lists it.
type entry struct {
	Metadata `yaml:",inline"`
	URLs     []string  `json:"urls" yaml:"urls"`
	Created  time.Time `json:"created" yaml:"created"`
	Digest   string    `json:"digest,omitempty" yaml:"digest,omitempty"`

	ver    Version
	origin string // proxy: absolute upstream URL of the archive
}

// view is the set of chart versions a repository serves, grouped by chart
// name, newest version first. Views are immutable once built; the rendered
// index.yaml is produced once per view.
type view struct {
	charts map[string][]*entry
	at     time.Time
	// blocks are the routing exclusions applied while building the view
	// (and the views it merges), one per rule; recorded on every serve.
	blocks []*core.RoutingBlock

	once sync.Once
	doc  []byte
	etag string
	err  error
}

// newView groups es by name. When two entries share a name and version the
// first one wins (group member order).
func newView(es []*entry, at time.Time) *view {
	v := &view{charts: map[string][]*entry{}, at: at}
	seen := map[[2]string]bool{}
	for _, e := range es {
		k := [2]string{e.Name, e.Version}
		if seen[k] {
			continue
		}
		seen[k] = true
		v.charts[e.Name] = append(v.charts[e.Name], e)
	}
	for _, list := range v.charts {
		sort.SliceStable(list, func(i, j int) bool { return list[i].ver.Compare(list[j].ver) > 0 })
	}
	return v
}

// all returns every entry, by name then newest first.
func (v *view) all() []*entry {
	names := make([]string, 0, len(v.charts))
	for n := range v.charts {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []*entry
	for _, n := range names {
		out = append(out, v.charts[n]...)
	}
	return out
}

// find returns the entry of name/version, nil if absent.
func (v *view) find(name, version string) *entry {
	for _, e := range v.charts[name] {
		if e.Version == version {
			return e
		}
	}
	return nil
}

type indexDoc struct {
	APIVersion string              `yaml:"apiVersion"`
	Entries    map[string][]*entry `yaml:"entries"`
	Generated  time.Time           `yaml:"generated"`
	ServerInfo map[string]string   `yaml:"serverInfo,omitempty"`
}

// render returns the index.yaml document of the view and its ETag.
func (v *view) render(contextPath string) ([]byte, string, error) {
	v.once.Do(func() {
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		doc := indexDoc{APIVersion: "v1", Entries: v.charts, Generated: v.at.UTC(),
			ServerInfo: map[string]string{"contextPath": contextPath}}
		if err := enc.Encode(doc); err != nil {
			v.err = fmt.Errorf("render index.yaml: %w", err)
			return
		}
		if err := enc.Close(); err != nil {
			v.err = fmt.Errorf("render index.yaml: %w", err)
			return
		}
		v.doc = buf.Bytes()
		sum := sha256.Sum256(v.doc)
		v.etag = `"` + hex.EncodeToString(sum[:16]) + `"`
	})
	return v.doc, v.etag, v.err
}

// exclusions collects the routing blocks applied to a view, keeping one per
// rule (repository, member, rule, pattern) so an allow list excluding
// thousands of remote charts reports once, not once per chart.
type exclusions struct {
	seen   map[[4]string]bool
	blocks []*core.RoutingBlock
}

func (x *exclusions) add(b *core.RoutingBlock) {
	k := [4]string{b.Repository, b.Member, b.Rule, b.Pattern}
	if x.seen == nil {
		x.seen = map[[4]string]bool{}
	}
	if !x.seen[k] {
		x.seen[k] = true
		x.blocks = append(x.blocks, b)
	}
}

// rulesKey fingerprints repo's routing rules for view cache keys, so a rule
// change takes effect on the next request.
func rulesKey(repo *core.Repository) string {
	if repo.Config.Routing.Empty() {
		return ""
	}
	b, err := json.Marshal(repo.Config.Routing)
	if err != nil {
		return "rules:?"
	}
	sum := sha256.Sum256(b)
	return "rules:" + hex.EncodeToString(sum[:8]) + ";"
}

// servedView returns repo's view and records the routing exclusions it
// applied (metric, X-Hangar-Routing header, audit).
func (h *handler) servedView(ctx context.Context, repo *core.Repository) (*view, error) {
	v, err := h.view(ctx, repo, 0)
	if err != nil {
		return nil, err
	}
	for _, b := range v.blocks {
		routing.Record(ctx, Name, b)
	}
	return v, nil
}

// view returns the chart versions repo serves. depth guards group nesting.
func (h *handler) view(ctx context.Context, repo *core.Repository, depth int) (*view, error) {
	switch repo.Type {
	case core.RepoHosted:
		return h.hostedView(ctx, repo)
	case core.RepoProxy:
		return h.proxyView(ctx, repo)
	case core.RepoGroup:
		return h.groupView(ctx, repo, depth)
	}
	return nil, fmt.Errorf("repository type %q: %w", repo.Type, core.ErrInvalid)
}

// hostedView lists the stored chart versions of a hosted repository.
func (h *handler) hostedView(ctx context.Context, repo *core.Repository) (*view, error) {
	if v, ok := h.views.get(repo, ""); ok {
		return v, nil
	}
	gen := h.views.generation(repo.Name)
	res, err, _ := h.flight.Do("view:"+repo.ID, func() (any, error) {
		comps, err := h.allComponents(context.WithoutCancel(ctx), repo, "")
		if err != nil {
			return nil, err
		}
		es := make([]*entry, 0, len(comps))
		for _, c := range comps {
			if c.Quarantined {
				continue
			}
			if e, ok := componentEntry(c); ok {
				es = append(es, e)
			}
		}
		v := newView(es, h.now())
		h.views.put(gen, repo, "", v)
		return v, nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*view), nil
}

// allComponents lists the components of repo, optionally of one chart.
func (h *handler) allComponents(ctx context.Context, repo *core.Repository, name string) ([]*core.Component, error) {
	empty := ""
	q := core.ComponentQuery{RepositoryID: repo.ID, Namespace: &empty, Name: name, Limit: core.MaxPageSize}
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

// componentEntry converts a stored chart version into an index entry.
func componentEntry(c *core.Component) (*entry, bool) {
	md, err := componentMetadata(c)
	if err != nil {
		return nil, false
	}
	ver, err := ParseVersion(md.Version)
	if err != nil {
		return nil, false
	}
	e := &entry{Metadata: *md, URLs: []string{chartPath(md.Name, md.Version)}, ver: ver, Created: c.CreatedAt.UTC()}
	e.Digest, _ = c.Attributes[attrDigest].(string)
	if s, ok := c.Attributes[attrCreated].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			e.Created = t.UTC()
		}
	}
	return e, true
}

// componentMetadata decodes the Chart.yaml metadata stored on a component.
func componentMetadata(c *core.Component) (*Metadata, error) {
	raw, ok := c.Attributes[attrChart]
	if !ok {
		return nil, fmt.Errorf("component %s has no chart metadata: %w", c.ID, core.ErrNotFound)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode chart metadata: %w", err)
	}
	var md Metadata
	if err := json.Unmarshal(b, &md); err != nil {
		return nil, fmt.Errorf("decode chart metadata: %w", err)
	}
	if md.Name == "" || md.Version == "" {
		return nil, fmt.Errorf("component %s: incomplete chart metadata: %w", c.ID, core.ErrInvalid)
	}
	return &md, nil
}

// groupView merges the views of a group's members in order.
func (h *handler) groupView(ctx context.Context, group *core.Repository, depth int) (*view, error) {
	if depth >= maxGroupDepth {
		return nil, fmt.Errorf("group %q nested too deeply: %w", group.Name, core.ErrInvalid)
	}
	members, err := h.deps.Repos.Members(ctx, group)
	if err != nil {
		return nil, err
	}
	var (
		views []*view
		names []string
		key   strings.Builder
		first error
		ok    int
	)
	key.WriteString(rulesKey(group))
	for _, m := range members {
		if m.Format != Name || !m.Online {
			continue
		}
		v, err := h.view(ctx, m, depth+1)
		if err != nil {
			h.log.WarnContext(ctx, "helm group member failed", "group", group.Name, "member", m.Name, "err", err)
			if first == nil {
				first = err
			}
			continue
		}
		ok++
		views = append(views, v)
		names = append(names, m.Name)
		fmt.Fprintf(&key, "%p;", v)
	}
	if ok == 0 && first != nil {
		return nil, first
	}
	if v, hit := h.views.get(group, key.String()); hit {
		return v, nil
	}
	gen := h.views.generation(group.Name)
	var (
		es []*entry
		ex exclusions
	)
	for i, v := range views {
		for _, b := range v.blocks {
			ex.add(b)
		}
		for _, e := range v.all() {
			if b := group.MemberRoutingBlock(names[i], e.Name); b != nil {
				ex.add(b)
				continue
			}
			es = append(es, e)
		}
	}
	v := newView(es, h.now())
	v.blocks = ex.blocks
	if first == nil {
		h.views.put(gen, group, key.String(), v)
	}
	return v, nil
}

// locate finds the archive or provenance file of name/version in repo,
// caching it from upstream in proxies, and returns the repository that
// holds it.
func (h *handler) locate(ctx context.Context, repo *core.Repository, name, version string, prov bool, depth int) (*core.Repository, *core.Asset, error) {
	path := chartPath(name, version)
	if prov {
		path = provPath(name, version)
	}
	switch repo.Type {
	case core.RepoHosted:
		a, err := h.deps.Components.GetAsset(ctx, repo.ID, path)
		return repo, a, err
	case core.RepoProxy:
		if err := routing.Check(ctx, repo, name); err != nil {
			return nil, nil, err
		}
		a, err := h.proxyAsset(ctx, repo, name, version, prov)
		return repo, a, err
	case core.RepoGroup:
		if depth >= maxGroupDepth {
			return nil, nil, fmt.Errorf("group %q nested too deeply: %w", repo.Name, core.ErrInvalid)
		}
		members, err := routing.Members(ctx, h.deps.Repos, repo, name)
		if err != nil {
			return nil, nil, err
		}
		var first error
		for _, m := range members {
			if m.Format != Name || !m.Online {
				continue
			}
			holder, a, err := h.locate(ctx, m, name, version, prov, depth+1)
			if err == nil {
				return holder, a, nil
			}
			if !errors.Is(err, core.ErrNotFound) && first == nil {
				h.log.WarnContext(ctx, "helm group member failed", "group", repo.Name, "member", m.Name, "err", err)
				first = err
			}
		}
		if first == nil {
			first = fmt.Errorf("%s: %w", path, core.ErrNotFound)
		}
		return nil, nil, first
	}
	return nil, nil, fmt.Errorf("repository type %q: %w", repo.Type, core.ErrInvalid)
}

// Defaults for the view cache.
const (
	// viewCacheTTL bounds how stale a hosted index can get should an
	// invalidation event be lost between nodes.
	viewCacheTTL = time.Minute
)

// viewCache keeps the view (and with it the rendered index.yaml) of each
// repository. Hosted views are dropped on component.* and repository.*
// events (from every node) and synchronously by this node's own writes; a
// per-repository generation stops a view built before an invalidation from
// being stored after it. Proxy views are keyed by the cached upstream index
// they were built from, group views by the member views they merge, so
// they are rebuilt exactly when an input changed.
type viewCache struct {
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	gens  map[string]uint64
	views map[string]*cachedView // by repository name
}

type cachedView struct {
	repoID string
	typ    core.RepoType
	key    string
	v      *view
	at     time.Time
}

func newViewCache(now func() time.Time) *viewCache {
	return &viewCache{ttl: viewCacheTTL, now: now, gens: map[string]uint64{}, views: map[string]*cachedView{}}
}

func (c *viewCache) get(repo *core.Repository, key string) (*view, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cv, ok := c.views[repo.Name]
	if !ok || c.ttl <= 0 || cv.repoID != repo.ID || cv.key != key {
		return nil, false
	}
	if repo.Type == core.RepoHosted && c.now().Sub(cv.at) > c.ttl {
		return nil, false
	}
	return cv.v, true
}

func (c *viewCache) generation(repo string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gens[repo]
}

func (c *viewCache) put(gen uint64, repo *core.Repository, key string, v *view) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl <= 0 || c.gens[repo.Name] != gen {
		return
	}
	c.views[repo.Name] = &cachedView{repoID: repo.ID, typ: repo.Type, key: key, v: v, at: c.now()}
}

// invalidate drops the view of the named repository. A proxy filling its
// cache (component.published) does not change what its index lists, so
// such events leave proxy views alone.
func (c *viewCache) invalidate(repo, eventType string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cv := c.views[repo]; cv != nil && cv.typ == core.RepoProxy && eventType == core.EventComponentPublished {
		return
	}
	c.gens[repo]++
	delete(c.views, repo)
}

func (c *viewCache) disable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl = 0
	c.views = map[string]*cachedView{}
}

// watch subscribes the cache to the events that change what a repository lists.
func (c *viewCache) watch(bus core.EventBus) error {
	if bus == nil {
		return nil
	}
	for _, pattern := range []string{"component.*", "repository.*"} {
		if _, err := bus.Subscribe(context.Background(), pattern, func(_ context.Context, e core.Event) {
			if e.Repository != "" {
				c.invalidate(e.Repository, e.Type)
			}
		}); err != nil {
			return err
		}
	}
	return nil
}
