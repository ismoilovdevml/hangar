package maven

import (
	"context"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Defaults for the generated-metadata cache.
const (
	// metaCacheTTL bounds staleness should an invalidation event be lost
	// (events are at-most-once between nodes).
	metaCacheTTL = time.Minute
	// metaCacheMax bounds the number of cached documents.
	metaCacheMax = 20000
)

// metaCache keeps the maven-metadata.xml documents generated for hosted
// repositories. Generating one lists the artifact's versions (or a SNAPSHOT
// directory) and reads stored plugin metadata; Maven and Gradle fetch the
// document and its checksum files on every resolution, and the checksums
// must describe exactly the bytes served. Serving the document and every
// checksum from one cached entity keeps them consistent by construction.
//
// Entries are dropped per repository on component.*, asset.published,
// asset.deleted and repository.* events (from every node), and synchronously
// by this node's own Maven writes. A per-repository generation stops a
// document generated before an invalidation from being stored after it.
type metaCache struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu     sync.Mutex
	size   int
	gens   map[string]uint64                // by repository name
	byRepo map[string]map[string]*metaEntry // repository name -> path -> entry
}

type metaEntry struct {
	repoID string
	e      entity
	at     time.Time
}

func newMetaCache() *metaCache {
	return &metaCache{ttl: metaCacheTTL, max: metaCacheMax, now: time.Now,
		gens: map[string]uint64{}, byRepo: map[string]map[string]*metaEntry{}}
}

func (c *metaCache) get(repo *core.Repository, path string) (*entity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	me, ok := c.byRepo[repo.Name][path]
	if !ok || c.ttl <= 0 || me.repoID != repo.ID || c.now().Sub(me.at) > c.ttl {
		return nil, false
	}
	e := me.e // the entity's byte slice and checksum map are never modified
	return &e, true
}

func (c *metaCache) generation(repo string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gens[repo]
}

func (c *metaCache) put(gen uint64, repo *core.Repository, path string, e *entity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl <= 0 || c.gens[repo.Name] != gen {
		return
	}
	m := c.byRepo[repo.Name]
	if m == nil {
		m = map[string]*metaEntry{}
		c.byRepo[repo.Name] = m
	}
	if _, ok := m[path]; !ok {
		if c.size >= c.max {
			c.evictOne()
		}
		c.size++
	}
	m[path] = &metaEntry{repoID: repo.ID, e: *e, at: c.now()}
}

// evictOne drops an arbitrary entry (map iteration order is random).
func (c *metaCache) evictOne() {
	for name, m := range c.byRepo {
		for p := range m {
			delete(m, p)
			c.size--
			if len(m) == 0 {
				delete(c.byRepo, name)
			}
			return
		}
	}
}

// disable turns the cache off.
func (c *metaCache) disable() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ttl = 0
	c.byRepo = map[string]map[string]*metaEntry{}
	c.size = 0
}

// invalidate drops every document of the named repository.
func (c *metaCache) invalidate(repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gens[repo]++
	c.size -= len(c.byRepo[repo])
	delete(c.byRepo, repo)
}

// watch subscribes the cache to the events that change hosted content.
func (c *metaCache) watch(bus core.EventBus) error {
	if bus == nil {
		return nil
	}
	for _, pattern := range []string{"component.*", core.EventAssetPublished, core.EventAssetDeleted, "repository.*"} {
		if _, err := bus.Subscribe(context.Background(), pattern, func(_ context.Context, e core.Event) {
			if e.Repository != "" {
				c.invalidate(e.Repository)
			}
		}); err != nil {
			return err
		}
	}
	return nil
}
