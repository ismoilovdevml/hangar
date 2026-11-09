// Package raw implements the Raw format: arbitrary files stored and served by
// path under /repository/{repo}/.
//
//	PUT    /repository/{repo}/{path}   upload (hosted)
//	GET    /repository/{repo}/{path}   download, with Range and conditional requests
//	HEAD   /repository/{repo}/{path}   headers only
//	DELETE /repository/{repo}/{path}   remove (hosted; evicts the cache in a proxy)
//	GET    /repository/{repo}/{dir}/   directory listing, HTML or JSON
//
// Component model: every file is one component whose namespace is the
// directory path ("" at the repository root), whose name is the file name and
// whose version is empty. The component has exactly one asset, the file.
//
// Proxy repositories fetch any path from their remote (an arbitrary HTTP
// server) and cache it; cached files are revalidated after the repository's
// content TTL. Group repositories serve a file from the first member that has
// it and list the union of their members' directories.
package raw

import (
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Name is the format identifier stored in repositories.format.
const Name = "raw"

// Format implements core.Format for Raw.
type Format struct {
	now func() time.Time
}

var _ core.Format = (*Format)(nil)

// New returns the Raw format.
func New() *Format { return &Format{now: time.Now} }

// Name implements core.Format.
func (f *Format) Name() string { return Name }

// Mount implements core.Format. Routes are relative to /repository/{repo}/.
func (f *Format) Mount(r chi.Router, deps core.Deps) {
	now := f.now
	if now == nil {
		now = time.Now
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	h := &handler{deps: deps, now: now, neg: newNegativeCache(now)}
	r.HandleFunc("/*", h.serveHTTP)
}

// Challenge implements core.Challenger: a Basic challenge.
func (f *Format) Challenge(w http.ResponseWriter, r *http.Request, deps core.Deps, err error) {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	(&handler{deps: deps}).fail(w, r, err)
}

// Validate implements core.Format. Raw repositories take no format settings.
func (f *Format) Validate(repo *core.Repository) error {
	if repo.Format != Name {
		return fmt.Errorf("repository %q has format %q, not %s: %w", repo.Name, repo.Format, Name, core.ErrInvalid)
	}
	if !repo.Type.Valid() {
		return fmt.Errorf("repository %q: unknown type %q: %w", repo.Name, repo.Type, core.ErrInvalid)
	}
	if len(repo.Config.Format) > 0 {
		return fmt.Errorf("repository %q: raw repositories have no format settings: %w", repo.Name, core.ErrInvalid)
	}
	if repo.Type == core.RepoProxy && repo.Config.Proxy != nil &&
		(repo.Config.Proxy.ContentTTL < 0 || repo.Config.Proxy.NegativeCacheTTL < 0) {
		return fmt.Errorf("repository %q: proxy TTLs must not be negative: %w", repo.Name, core.ErrInvalid)
	}
	return nil
}

// Coordinates implements core.Format: namespace is the directory, name the file.
func (f *Format) Coordinates(a *core.Asset) core.Coordinates {
	dir, name := splitPath(core.CleanPath(a.Path))
	return core.Coordinates{Format: Name, Namespace: dir, Name: name}
}

// handler serves every repository of this format.
type handler struct {
	deps core.Deps
	now  func() time.Time
	neg  *negativeCache
	fill singleflight.Group
}

// negativeCache remembers upstream 404s per repository path until they expire.
type negativeCache struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]time.Time
}

// negativeCacheMax bounds memory: when full, the cache is cleared.
const negativeCacheMax = 10000

func newNegativeCache(now func() time.Time) *negativeCache {
	return &negativeCache{now: now, m: map[string]time.Time{}}
}

func (c *negativeCache) has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.m[key]
	if ok && c.now().After(exp) {
		delete(c.m, key)
		return false
	}
	return ok
}

func (c *negativeCache) add(key string, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= negativeCacheMax {
		clear(c.m)
	}
	c.m[key] = c.now().Add(ttl)
}

func (c *negativeCache) forget(key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}
