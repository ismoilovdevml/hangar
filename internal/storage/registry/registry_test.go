package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
	"github.com/ismoilovdevml/hangar/internal/storage/fs"
	"github.com/ismoilovdevml/hangar/internal/storage/registry"
)

func hostedOn(store string) *core.Repository {
	return &core.Repository{Name: "raw-h", Format: "raw", Type: core.RepoHosted, BlobStore: store}
}

func TestFSStoreResolvedAndCached(t *testing.T) {
	pool := pgtest.Pool(t)
	dir := t.TempDir()
	pgtest.AddBlobStore(t, pool, "local", "fs", map[string]any{"path": dir})
	now := time.Unix(1_700_000_000, 0)
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	reg := registry.New(pool, registry.Options{RecheckAfter: time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()) }})
	ctx := context.Background()

	s1, err := reg.ForRepository(ctx, hostedOn("local"))
	if err != nil {
		t.Fatal(err)
	}
	fsStore, ok := s1.(*fs.Store)
	if !ok || fsStore.Root() != dir {
		t.Fatalf("store = %T %v", s1, s1)
	}
	d, _, err := s1.Put(ctx, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Stat(ctx, d); err != nil {
		t.Fatal(err)
	}
	s2, _ := reg.ByName(ctx, "local")
	if s1 != s2 {
		t.Fatal("store not cached")
	}
	// After RecheckAfter an unchanged row keeps the same instance.
	clock.Add(int64(2 * time.Minute))
	if s3, _ := reg.ByName(ctx, "local"); s3 != s1 {
		t.Fatal("unchanged row rebuilt the store")
	}
	// A changed row is picked up after the recheck interval.
	dir2 := t.TempDir()
	if _, err := pool.Exec(ctx, `UPDATE blob_stores SET config = $1, updated_at = now() + interval '1 second' WHERE name = 'local'`,
		map[string]any{"path": dir2}); err != nil {
		t.Fatal(err)
	}
	if s4, _ := reg.ByName(ctx, "local"); s4 != s1 {
		t.Fatal("cache ignored RecheckAfter")
	}
	clock.Add(int64(2 * time.Minute))
	s5, err := reg.ByName(ctx, "local")
	if err != nil || s5.(*fs.Store).Root() != dir2 {
		t.Fatalf("changed row not reloaded: %v %v", s5, err)
	}
	// Invalidate forces a re-read immediately.
	if _, err := pool.Exec(ctx, `DELETE FROM blob_stores WHERE name = 'local'`); err != nil {
		t.Fatal(err)
	}
	reg.Invalidate("local")
	if _, err := reg.ByName(ctx, "local"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("deleted store = %v, want ErrNotFound", err)
	}
}

func TestFallbackStaticAndErrors(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	fallback := coretest.NewMemBlobStore()
	pinned := coretest.NewMemBlobStore()
	reg := registry.New(pool, registry.Options{Fallback: fallback, Static: map[string]core.BlobStore{"default": pinned}})

	tests := []struct {
		name string
		repo *core.Repository
		want core.BlobStore
		err  error
	}{
		{"static", hostedOn("default"), pinned, nil},
		{"unknown name falls back", hostedOn("nowhere"), fallback, nil},
		{"empty name falls back", hostedOn(""), fallback, nil},
		{"group", &core.Repository{Name: "raw-g", Type: core.RepoGroup}, nil, core.ErrInvalid},
		{"nil", nil, nil, core.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := reg.ForRepository(ctx, tt.repo)
			if !errors.Is(err, tt.err) || got != tt.want {
				t.Fatalf("ForRepository = %v, %v; want %v, %v", got, err, tt.want, tt.err)
			}
		})
	}
	strict := registry.New(pool, registry.Options{})
	if _, err := strict.ByName(ctx, "nowhere"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("no fallback = %v, want ErrNotFound", err)
	}
	// A kind without a registered driver is reported, not silently replaced.
	pgtest.AddBlobStore(t, pool, "objects", "s3", map[string]any{"bucket": "b"})
	if _, err := reg.ByName(ctx, "objects"); !errors.Is(err, core.ErrNotImplemented) {
		t.Errorf("missing driver = %v, want ErrNotImplemented", err)
	}
	// A broken fs config fails with ErrInvalid.
	pgtest.AddBlobStore(t, pool, "broken", "fs", map[string]any{"path": ""})
	if _, err := reg.ByName(ctx, "broken"); !errors.Is(err, core.ErrInvalid) {
		t.Errorf("empty fs path = %v, want ErrInvalid", err)
	}
}

func TestDriverFactories(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	pgtest.AddBlobStore(t, pool, "objects", "s3", map[string]any{"bucket": "artifacts", "region": "eu-1"})

	var calls atomic.Int32
	var seen registry.StoreConfig
	mem := coretest.NewMemBlobStore()
	factory := func(_ context.Context, cfg registry.StoreConfig) (core.BlobStore, error) {
		calls.Add(1)
		seen = cfg
		time.Sleep(20 * time.Millisecond) // widen the window for concurrent lookups
		return mem, nil
	}
	reg := registry.New(pool, registry.Options{Drivers: map[string]registry.Factory{"s3": factory}})
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if s, err := reg.ByName(ctx, "objects"); err != nil || s != mem {
				t.Errorf("ByName = %v, %v", s, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("factory called %d times, want 1", calls.Load())
	}
	var cfg map[string]string
	if err := json.Unmarshal(seen.Config, &cfg); err != nil || cfg["bucket"] != "artifacts" || seen.Kind != "s3" || seen.Name != "objects" || seen.ID == "" {
		t.Errorf("factory got %+v (%v)", seen, err)
	}

	// Process-wide registration applies to registries created afterwards.
	registry.RegisterDriver("s3", func(context.Context, registry.StoreConfig) (core.BlobStore, error) { return mem, nil })
	t.Cleanup(func() { registry.RegisterDriver("s3", nil) })
	if s, err := registry.New(pool, registry.Options{}).ByName(ctx, "objects"); err != nil || s != mem {
		t.Errorf("registered driver: %v, %v", s, err)
	}
}

func TestEnsureStore(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	id1, err := registry.EnsureStore(ctx, pool, "default", "fs", registry.FSConfig{Path: "/var/lib/hangar/blobs"})
	if err != nil || id1 == "" {
		t.Fatalf("EnsureStore = %q, %v", id1, err)
	}
	id2, err := registry.EnsureStore(ctx, pool, "default", "fs", registry.FSConfig{Path: "/elsewhere"})
	if err != nil || id2 != id1 {
		t.Fatalf("second EnsureStore = %q, %v; want %q", id2, err, id1)
	}
	var path string
	if err := pool.QueryRow(ctx, `SELECT config->>'path' FROM blob_stores WHERE name = 'default'`).Scan(&path); err != nil || path != "/var/lib/hangar/blobs" {
		t.Fatalf("existing row changed: %q, %v", path, err)
	}
	if _, err := registry.EnsureStore(ctx, pool, "bad", "ftp", nil); err == nil {
		t.Fatal("unknown kind accepted")
	}
}

func TestConformanceThroughRegistry(t *testing.T) {
	pool := pgtest.Pool(t)
	n := 0
	coretest.RunBlobStoreTests(t, func(t *testing.T) core.BlobStore {
		n++
		name := "fs-" + string(rune('a'+n))
		pgtest.AddBlobStore(t, pool, name, "fs", map[string]any{"path": t.TempDir()})
		s, err := registry.New(pool, registry.Options{}).ByName(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
