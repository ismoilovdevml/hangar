package repository

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// repoCache keeps recently read repositories and group member lists. Every
// format request resolves its repository (and groups their members), so this
// saves one or two queries per request.
//
// A generation counter guards against caching a value read before an
// invalidation: readers note the generation before querying and put only
// stores the result if no invalidation happened in between.
type repoCache struct {
	ttl time.Duration

	mu      sync.Mutex
	gen     uint64
	byName  map[string]cachedRepo
	idName  map[string]string
	members map[string]cachedMembers // by group id
}

type cachedRepo struct {
	repo *core.Repository
	at   time.Time
}

type cachedMembers struct {
	repos []*core.Repository
	at    time.Time
}

func newRepoCache(ttl time.Duration) *repoCache {
	return &repoCache{ttl: ttl, byName: map[string]cachedRepo{}, idName: map[string]string{}, members: map[string]cachedMembers{}}
}

func (c *repoCache) enabled() bool { return c.ttl > 0 }

func (c *repoCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

func (c *repoCache) get(name string) (*core.Repository, bool) {
	if !c.enabled() {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byName[name]
	if !ok || time.Since(e.at) > c.ttl {
		return nil, false
	}
	return cloneRepo(e.repo), true
}

func (c *repoCache) getByID(id string) (*core.Repository, bool) {
	if !c.enabled() {
		return nil, false
	}
	c.mu.Lock()
	name, ok := c.idName[strings.ToLower(id)]
	c.mu.Unlock()
	if !ok {
		return nil, false
	}
	return c.get(name)
}

func (c *repoCache) put(gen uint64, r *core.Repository) {
	if !c.enabled() || r == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	c.byName[r.Name] = cachedRepo{repo: cloneRepo(r), at: time.Now()}
	c.idName[strings.ToLower(r.ID)] = r.Name
}

func (c *repoCache) getMembers(groupID string) ([]*core.Repository, bool) {
	if !c.enabled() {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.members[strings.ToLower(groupID)]
	if !ok || time.Since(e.at) > c.ttl {
		return nil, false
	}
	return cloneRepos(e.repos), true
}

func (c *repoCache) putMembers(gen uint64, groupID string, repos []*core.Repository) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	c.members[strings.ToLower(groupID)] = cachedMembers{repos: cloneRepos(repos), at: time.Now()}
}

// invalidate drops everything. Repository changes are rare, so a full flush
// is simpler than tracking which groups list which members.
func (c *repoCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	clear(c.byName)
	clear(c.idName)
	clear(c.members)
}

func cloneRepos(in []*core.Repository) []*core.Repository {
	out := make([]*core.Repository, len(in))
	for i, r := range in {
		out[i] = cloneRepo(r)
	}
	return out
}

// Invalidate drops every cached repository. Watch calls it for repository
// events from any node.
func (s *Service) Invalidate() { s.cache.invalidate() }

// Watch subscribes to repository.* events on bus, so repository changes made
// on other nodes invalidate this node's cache. Unsubscribe on shutdown.
func (s *Service) Watch(ctx context.Context, bus core.EventBus) (core.Subscription, error) {
	return bus.Subscribe(ctx, "repository.*", func(context.Context, core.Event) { s.cache.invalidate() })
}

// cloneFormat copies the format-specific settings map, so a caller adding or
// removing keys does not change the cached value.
func cloneFormat(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return maps.Clone(m)
}
