// Package registry resolves the blob store of a repository from the
// blob_stores table. It implements core.BlobStores.
//
// Each blob_stores row names a driver kind ("fs", "s3") and a JSON config.
// Drivers are looked up in a factory map: "fs" is built in, other drivers
// register themselves with RegisterDriver (process-wide) or Options.Drivers
// (per registry). Opened stores are cached by name and rebuilt when the row's
// updated_at changes, which is re-checked at most every Options.RecheckAfter.
//
// Stores provided by the process configuration can be pinned by name with
// Options.Static, and Options.Fallback serves repositories whose store name
// is empty or has no row — the single-store behaviour of storage.Single.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/storage/fs"
)

// StoreConfig is one blob_stores row as handed to a driver factory.
type StoreConfig struct {
	ID     string
	Name   string
	Kind   string
	Config json.RawMessage // the row's config JSON object
}

// Factory opens a blob store driver from its configuration.
type Factory func(ctx context.Context, cfg StoreConfig) (core.BlobStore, error)

// FSConfig is the config JSON of an "fs" blob store.
type FSConfig struct {
	// Path is the root directory of the store.
	Path string `json:"path"`
}

// OpenFS is the factory for kind "fs".
func OpenFS(_ context.Context, cfg StoreConfig) (core.BlobStore, error) {
	var c FSConfig
	if len(cfg.Config) > 0 {
		if err := json.Unmarshal(cfg.Config, &c); err != nil {
			return nil, fmt.Errorf("blob store %q: fs config: %w: %w", cfg.Name, err, core.ErrInvalid)
		}
	}
	s, err := fs.New(c.Path)
	if err != nil {
		return nil, fmt.Errorf("blob store %q: %w", cfg.Name, err)
	}
	return s, nil
}

var (
	driversMu sync.RWMutex
	drivers   = map[string]Factory{"fs": OpenFS}
)

// RegisterDriver makes a driver kind available to every Registry created
// afterwards. Registering a kind again replaces the factory; a nil factory
// removes the kind.
func RegisterDriver(kind string, f Factory) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if f == nil {
		delete(drivers, kind)
		return
	}
	drivers[kind] = f
}

// DefaultRecheck is how long a cached store is used before its row is re-read.
const DefaultRecheck = 30 * time.Second

// Options configures a Registry. Every field is optional.
type Options struct {
	// Drivers adds or overrides factories for this registry only.
	Drivers map[string]Factory
	// Static pins stores by name; they are used without consulting the database.
	Static map[string]core.BlobStore
	// Fallback serves repositories whose blob store name is empty or not in
	// the blob_stores table. Nil makes those lookups fail with ErrNotFound.
	Fallback core.BlobStore
	// RecheckAfter bounds how stale a cached store may be. Default 30s.
	RecheckAfter time.Duration
	Logger       *slog.Logger
	// Now is the clock (tests).
	Now func() time.Time
}

// Registry implements core.BlobStores over the blob_stores table.
type Registry struct {
	pool    *pgxpool.Pool
	opts    Options
	log     *slog.Logger
	drivers map[string]Factory

	mu    sync.Mutex
	cache map[string]*entry
	sf    singleflight.Group
}

type entry struct {
	store     core.BlobStore
	updatedAt time.Time
	checkedAt time.Time
}

var _ core.BlobStores = (*Registry)(nil)

// New returns a registry reading blob_stores through pool.
func New(pool *pgxpool.Pool, opts Options) *Registry {
	if opts.RecheckAfter <= 0 {
		opts.RecheckAfter = DefaultRecheck
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Registry{pool: pool, opts: opts, log: log, drivers: map[string]Factory{}, cache: map[string]*entry{}}
	driversMu.RLock()
	for k, f := range drivers {
		r.drivers[k] = f
	}
	driversMu.RUnlock()
	for k, f := range opts.Drivers {
		r.drivers[k] = f
	}
	return r
}

// ForRepository implements core.BlobStores.
func (r *Registry) ForRepository(ctx context.Context, repo *core.Repository) (core.BlobStore, error) {
	if repo == nil {
		return nil, fmt.Errorf("blob store for nil repository: %w", core.ErrInvalid)
	}
	if repo.Type == core.RepoGroup {
		return nil, fmt.Errorf("group %q has no blob store; resolve a member: %w", repo.Name, core.ErrInvalid)
	}
	return r.ByName(ctx, repo.BlobStore)
}

// ByName returns the blob store called name.
func (r *Registry) ByName(ctx context.Context, name string) (core.BlobStore, error) {
	if s, ok := r.opts.Static[name]; ok {
		return s, nil
	}
	if name == "" {
		return r.fallback(name)
	}
	r.mu.Lock()
	e := r.cache[name]
	if e != nil && r.opts.Now().Sub(e.checkedAt) < r.opts.RecheckAfter {
		r.mu.Unlock()
		return e.store, nil
	}
	r.mu.Unlock()

	v, err, _ := r.sf.Do(name, func() (any, error) { return r.load(ctx, name) })
	if errors.Is(err, core.ErrNotFound) {
		return r.fallback(name)
	}
	if err != nil {
		return nil, err
	}
	return v.(core.BlobStore), nil
}

func (r *Registry) fallback(name string) (core.BlobStore, error) {
	if r.opts.Fallback != nil {
		return r.opts.Fallback, nil
	}
	return nil, fmt.Errorf("blob store %q: %w", name, core.ErrNotFound)
}

// load reads the row and reuses the cached store when the row is unchanged.
func (r *Registry) load(ctx context.Context, name string) (core.BlobStore, error) {
	var (
		cfg     StoreConfig
		updated time.Time
	)
	err := r.pool.QueryRow(ctx, `SELECT id::text, name, kind, config, updated_at FROM blob_stores WHERE name = $1`, name).
		Scan(&cfg.ID, &cfg.Name, &cfg.Kind, &cfg.Config, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		r.Invalidate(name)
		return nil, fmt.Errorf("blob store %q: %w", name, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("blob store %q: %w", name, err)
	}
	now := r.opts.Now()
	r.mu.Lock()
	if e := r.cache[name]; e != nil && e.updatedAt.Equal(updated) {
		e.checkedAt = now
		r.mu.Unlock()
		return e.store, nil
	}
	r.mu.Unlock()

	f, ok := r.drivers[cfg.Kind]
	if !ok {
		return nil, fmt.Errorf("blob store %q: driver %q is not available in this build: %w", name, cfg.Kind, core.ErrNotImplemented)
	}
	store, err := f(ctx, cfg)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cache[name] = &entry{store: store, updatedAt: updated, checkedAt: now}
	r.mu.Unlock()
	r.log.Info("blob store opened", "name", name, "kind", cfg.Kind)
	return store, nil
}

// Invalidate drops the cached store for name, so the next lookup re-reads
// its row. Call it after changing or deleting a blob store.
func (r *Registry) Invalidate(name string) {
	r.mu.Lock()
	delete(r.cache, name)
	r.mu.Unlock()
}

// EnsureStore creates the blob_stores row name with kind and config if no
// row of that name exists, and returns its id. An existing row is left as is.
// Startup uses it to register the store from the server configuration, so
// repositories can reference it.
func EnsureStore(ctx context.Context, pool *pgxpool.Pool, name, kind string, config any) (string, error) {
	if config == nil {
		config = map[string]any{}
	}
	var id string
	err := pool.QueryRow(ctx, `
WITH ins AS (
    INSERT INTO blob_stores (name, kind, config) VALUES ($1, $2, $3)
    ON CONFLICT (name) DO NOTHING RETURNING id
)
SELECT id::text FROM ins
UNION ALL
SELECT id::text FROM blob_stores WHERE name = $1
LIMIT 1`, name, kind, config).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("ensure blob store %q: %w", name, err)
	}
	return id, nil
}
