package blobgc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/db"
	"github.com/ismoilovdevml/hangar/internal/storage/fs"
)

const envTestDatabaseURL = "HANGAR_TEST_DATABASE_URL"

// isolatedPool connects to the test database inside a fresh, migrated schema
// that is dropped afterwards.
func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv(envTestDatabaseURL)
	if base == "" {
		t.Skipf("%s not set; skipping database integration test", envTestDatabaseURL)
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, base, db.PoolOptions{MaxConns: 2})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "hangar_test_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := db.Open(ctx, u.String(), db.PoolOptions{MaxConns: 6})
	if err != nil {
		t.Fatalf("connect with schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close()
	})
	m, err := db.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if err := m.Up(ctx, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	store   *fs.Store
	storeID string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := isolatedPool(t)
	st, err := fs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, pool: pool, store: st}
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO blob_stores (name, kind, config) VALUES ('default', 'fs', '{}') RETURNING id::text`).Scan(&f.storeID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) stores(s core.BlobStore) Stores {
	return StoresFunc(func(_ context.Context, id string) (core.BlobStore, error) {
		if id != f.storeID {
			return nil, core.ErrNotFound
		}
		return s, nil
	})
}

// put stores content and backdates the file by age.
func (f *fixture) put(content string, age time.Duration) core.Digest {
	f.t.Helper()
	d, _, err := f.store.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		f.t.Fatal(err)
	}
	if age > 0 {
		p := filepath.Join(f.store.Root(), "blobs", "sha256", d.Hex()[:2], d.Hex())
		past := time.Now().Add(-age)
		if err := os.Chtimes(p, past, past); err != nil {
			f.t.Fatal(err)
		}
	}
	return d
}

// row inserts a blobs row. softAgo < 0 leaves soft_deleted_at NULL.
func (f *fixture) row(d core.Digest, size int64, refs int, softAgo, createdAgo time.Duration) {
	f.t.Helper()
	var soft any
	if softAgo >= 0 {
		soft = time.Now().Add(-softAgo)
	}
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO blobs (store_id, digest, size, ref_count, soft_deleted_at, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		f.storeID, string(d), size, refs, soft, time.Now().Add(-createdAgo)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) hasRow(d core.Digest) bool {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM blobs WHERE store_id = $1 AND digest = $2`,
		f.storeID, string(d)).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n == 1
}

func (f *fixture) hasBytes(d core.Digest) bool {
	f.t.Helper()
	_, err := f.store.Stat(context.Background(), d)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		f.t.Fatal(err)
	}
	return err == nil
}

const (
	day  = 24 * time.Hour
	none = -1 * time.Second
)

func TestCollect(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	expired := f.put("expired", 3*day)
	f.row(expired, 7, 0, 2*day, 3*day)
	recent := f.put("recently unreferenced", 3*day)
	f.row(recent, 21, 0, time.Hour, 3*day)
	live := f.put("live", 3*day)
	f.row(live, 4, 2, none, 3*day)
	neverRef := f.put("never referenced", 3*day)
	f.row(neverRef, 16, 0, none, 3*day)
	youngNeverRef := f.put("young never referenced", 0)
	f.row(youngNeverRef, 22, 0, none, 0)
	missing := core.SHA256Of([]byte("bytes already gone"))
	f.row(missing, 18, 0, 2*day, 3*day)
	orphan := f.put("orphan", 3*day)
	youngOrphan := f.put("young orphan", 0)
	// A stray file in the blob tree is not a blob.
	if err := os.WriteFile(filepath.Join(f.store.Root(), "blobs", "sha256", "README"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := New(f.pool, f.stores(f.store), Options{GracePeriod: day, BatchSize: 1})
	var progress []Progress
	res, err := c.Run(ctx, Request{OrphanScan: true, OnProgress: func(p Progress) { progress = append(progress, p) }})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Stores) != 1 {
		t.Fatalf("stores = %d", len(res.Stores))
	}
	sr := res.Stores[0]
	if sr.Deleted != 3 || sr.Missing != 1 || sr.FreedBytes != 7+16 || sr.Failed != 0 || sr.Batches != 3 {
		t.Fatalf("result = %+v", sr)
	}
	if res.Deleted != 3 || res.FreedBytes != 23 {
		t.Fatalf("totals = %+v", res)
	}
	if len(progress) != 3 || progress[2].Deleted != 3 || progress[2].StoresTotal != 1 {
		t.Fatalf("progress = %+v", progress)
	}

	for _, d := range []core.Digest{expired, neverRef, missing} {
		if f.hasRow(d) || f.hasBytes(d) {
			t.Errorf("%s not collected (row=%v bytes=%v)", d, f.hasRow(d), f.hasBytes(d))
		}
	}
	for _, d := range []core.Digest{recent, live, youngNeverRef} {
		if !f.hasRow(d) || !f.hasBytes(d) {
			t.Errorf("%s wrongly collected (row=%v bytes=%v)", d, f.hasRow(d), f.hasBytes(d))
		}
	}

	o := sr.Orphans
	if o == nil {
		t.Fatal("no orphan report")
	}
	// Remaining blobs: recent, live, youngNeverRef (rows), orphan, youngOrphan.
	if o.Scanned != 5 || o.Young != 2 || o.Orphans != 1 || o.OrphanBytes != 6 || o.Truncated {
		t.Fatalf("orphans = %+v", o)
	}
	if len(o.Sample) != 1 || o.Sample[0].Digest != orphan {
		t.Fatalf("orphan sample = %+v", o.Sample)
	}
	if !f.hasBytes(orphan) || !f.hasBytes(youngOrphan) {
		t.Fatal("orphan scan deleted bytes")
	}

	// Idempotent: a second run finds nothing.
	res, err = c.Run(ctx, Request{})
	if err != nil || res.Deleted != 0 || res.Stores[0].Batches != 0 {
		t.Fatalf("second run = %+v, %v", res, err)
	}
}

func TestSkipsLockedRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a := f.put("a", 3*day)
	f.row(a, 1, 0, 2*day, 3*day)
	b := f.put("b", 3*day)
	f.row(b, 1, 0, 2*day, 3*day)

	// A concurrent publish holds the row lock on a.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM blobs WHERE store_id = $1 AND digest = $2 FOR UPDATE`, f.storeID, string(a)); err != nil {
		t.Fatal(err)
	}

	c := New(f.pool, f.stores(f.store), Options{GracePeriod: day})
	res, err := c.Run(ctx, Request{StoreID: f.storeID})
	if err != nil || res.Deleted != 1 {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if !f.hasRow(a) || !f.hasBytes(a) || f.hasRow(b) {
		t.Fatal("locked row collected or unlocked row kept")
	}

	// The publish references a and commits: never collected.
	if _, err := tx.Exec(ctx, `UPDATE blobs SET ref_count = 1, soft_deleted_at = NULL WHERE store_id = $1 AND digest = $2`,
		f.storeID, string(a)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if res, err := c.Run(ctx, Request{}); err != nil || res.Deleted != 0 {
		t.Fatalf("Run after publish = %+v, %v", res, err)
	}
	if !f.hasBytes(a) {
		t.Fatal("referenced blob deleted")
	}
}

// failingStore fails Delete for chosen digests.
type failingStore struct {
	core.BlobStore
	fail map[core.Digest]bool
}

func (s failingStore) Delete(ctx context.Context, d core.Digest) error {
	if s.fail[d] {
		return errors.New("store unavailable")
	}
	return s.BlobStore.Delete(ctx, d)
}

func TestDeleteFailureKeepsRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := f.put("bad", 3*day)
	f.row(bad, 3, 0, 2*day, 3*day)
	good := f.put("good", 3*day)
	f.row(good, 4, 0, 2*day, 3*day)

	c := New(f.pool, f.stores(failingStore{BlobStore: f.store, fail: map[core.Digest]bool{bad: true}}),
		Options{GracePeriod: day, BatchSize: 1})
	res, err := c.Run(ctx, Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Deleted != 1 || res.Failed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if !f.hasRow(bad) || !f.hasBytes(bad) || f.hasRow(good) {
		t.Fatal("failed blob's row removed or good blob kept")
	}
}

func TestStoreDownStops(t *testing.T) {
	f := newFixture(t)
	fail := map[core.Digest]bool{}
	for i := range 10 {
		d := f.put(strings.Repeat("x", i+1), 3*day)
		f.row(d, int64(i+1), 0, 2*day, 3*day)
		fail[d] = true
	}
	c := New(f.pool, f.stores(failingStore{BlobStore: f.store, fail: fail}), Options{GracePeriod: day, BatchSize: 2})
	res, err := c.Run(context.Background(), Request{})
	if err == nil {
		t.Fatal("Run succeeded with every delete failing")
	}
	if res.Failed != 2*maxFruitlessBatches || res.Stores[0].Error == "" {
		t.Fatalf("result = %+v", res)
	}
}

func TestHandlerAndScanOrphans(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d := f.put("old", 3*day)
	f.row(d, 3, 0, 2*day, 3*day)
	orphan := f.put("orphan", 3*day)

	c := New(f.pool, f.stores(f.store), Options{GracePeriod: day})
	payload, _ := json.Marshal(Request{StoreID: f.storeID})
	if err := c.Handler()(ctx, core.Job{ID: "j1", Type: JobType, Payload: payload}); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if f.hasRow(d) {
		t.Fatal("handler did not collect")
	}
	if err := c.Handler()(ctx, core.Job{Type: JobType, Payload: []byte("{")}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad payload = %v", err)
	}
	if _, err := c.Run(ctx, Request{StoreID: "00000000-0000-0000-0000-000000000000"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown store = %v", err)
	}

	rep, err := c.ScanOrphans(ctx, f.storeID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Orphans != 1 || rep.Sample[0].Digest != orphan {
		t.Fatalf("report = %+v", rep)
	}
	mem := New(f.pool, f.stores(coretest.NewMemBlobStore()), Options{})
	if _, err := mem.ScanOrphans(ctx, f.storeID, 0); !errors.Is(err, core.ErrNotImplemented) {
		t.Fatalf("scan of unlistable store = %v", err)
	}
}
