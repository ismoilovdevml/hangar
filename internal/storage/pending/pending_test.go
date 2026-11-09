package pending_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
	"github.com/ismoilovdevml/hangar/internal/storage/pending"
	"github.com/ismoilovdevml/hangar/internal/tasks/blobgc"
)

type fixture struct {
	pool    *pgxpool.Pool
	mem     *coretest.MemBlobStore
	storeID string
	repo    *core.Repository
	bs      core.BlobStore // the wrapped store of repo
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.Pool(t)
	f := &fixture{pool: pool, mem: coretest.NewMemBlobStore()}
	f.storeID = pgtest.AddBlobStore(t, pool, "default", "fs", nil)
	f.repo = &core.Repository{Name: "raw-hosted", Format: "raw", Type: core.RepoHosted, BlobStore: "default"}
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO repositories (name, format, type, blob_store_id) VALUES ($1, 'raw', 'hosted', $2) RETURNING id::text`,
		f.repo.Name, f.storeID).Scan(&f.repo.ID); err != nil {
		t.Fatal(err)
	}
	bs, err := pending.Wrap(pool, coretest.SingleStore{Store: f.mem}).ForRepository(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	f.bs = bs
	return f
}

type row struct {
	ref         int64
	size        int64
	softDeleted *time.Time
}

func (f *fixture) row(t *testing.T, d core.Digest) (row, bool) {
	t.Helper()
	var r row
	err := f.pool.QueryRow(context.Background(),
		`SELECT ref_count, size, soft_deleted_at FROM blobs WHERE store_id = $1 AND digest = $2`, f.storeID, string(d)).
		Scan(&r.ref, &r.size, &r.softDeleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return r, true
}

func (f *fixture) gc(t *testing.T) blobgc.Result {
	t.Helper()
	c := blobgc.New(f.pool, blobgc.StoresFunc(func(context.Context, string) (core.BlobStore, error) { return f.mem, nil }),
		blobgc.Options{GracePeriod: 24 * time.Hour, TempMaxAge: -1})
	res, err := c.Run(context.Background(), blobgc.Request{StoreID: f.storeID})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestPutRecordsUnreferencedBlob(t *testing.T) {
	f := newFixture(t)
	d, n, err := f.bs.Put(context.Background(), strings.NewReader("rejected content"))
	if err != nil {
		t.Fatal(err)
	}
	r, ok := f.row(t, d)
	if !ok || r.ref != 0 || r.size != n || r.softDeleted == nil || time.Since(*r.softDeleted) > time.Minute {
		t.Fatalf("row = %+v (exists %v), want unreferenced since now with size %d", r, ok, n)
	}
}

// Content that is stored but never published (a docker digest mismatch, an
// upload over a limit) is reclaimed by GC after its grace period.
func TestRejectedUploadIsCollected(t *testing.T) {
	f := newFixture(t)
	d, _, err := f.bs.Put(context.Background(), strings.NewReader("never published"))
	if err != nil {
		t.Fatal(err)
	}
	if res := f.gc(t); res.Deleted != 0 {
		t.Fatalf("gc inside the grace period deleted %d blobs", res.Deleted)
	}
	if _, err := f.mem.Stat(context.Background(), d); err != nil {
		t.Fatalf("bytes gone inside the grace period: %v", err)
	}
	f.exec(t, `UPDATE blobs SET soft_deleted_at = now() - interval '72 hours' WHERE digest = $1`, string(d))
	if res := f.gc(t); res.Deleted != 1 {
		t.Fatalf("gc after the grace period = %+v, want 1 deleted", res)
	}
	if _, err := f.mem.Stat(context.Background(), d); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("bytes after gc: %v, want ErrNotFound", err)
	}
	if _, ok := f.row(t, d); ok {
		t.Fatal("row left after gc")
	}
}

func TestPutLeavesReferencedRowAlone(t *testing.T) {
	f := newFixture(t)
	d := core.SHA256Of([]byte("published"))
	f.exec(t, `INSERT INTO blobs (store_id, digest, size, ref_count) VALUES ($1, $2, 9, 2)`, f.storeID, string(d))
	if _, _, err := f.bs.Put(context.Background(), strings.NewReader("published")); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.row(t, d); r.ref != 2 || r.softDeleted != nil {
		t.Fatalf("referenced row changed: %+v", r)
	}
}

// Uploading content again restarts the grace period of its unreferenced row,
// so GC does not delete it before the publish that follows.
func TestPutRestartsGracePeriod(t *testing.T) {
	f := newFixture(t)
	d := core.SHA256Of([]byte("uploaded again"))
	f.exec(t, `INSERT INTO blobs (store_id, digest, size, ref_count, soft_deleted_at) VALUES ($1, $2, 14, 0, now() - interval '72 hours')`,
		f.storeID, string(d))
	if _, _, err := f.bs.Put(context.Background(), strings.NewReader("uploaded again")); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.row(t, d); r.softDeleted == nil || time.Since(*r.softDeleted) > time.Minute {
		t.Fatalf("grace period not restarted: %+v", r)
	}
	if res := f.gc(t); res.Deleted != 0 {
		t.Fatalf("gc deleted %d blobs after a fresh upload", res.Deleted)
	}
	if _, err := f.mem.Stat(context.Background(), d); err != nil {
		t.Fatalf("bytes: %v", err)
	}
}

// A repository without an id (built in memory) is resolved by store name.
func TestPutResolvesStoreByName(t *testing.T) {
	f := newFixture(t)
	bs, err := pending.Wrap(f.pool, coretest.SingleStore{Store: f.mem}).
		ForRepository(context.Background(), &core.Repository{Name: "x", Type: core.RepoHosted, BlobStore: "default"})
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := bs.Put(context.Background(), strings.NewReader("by name"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f.row(t, d); !ok {
		t.Fatal("no row recorded")
	}
}

// The row is written even when the request is cancelled right after the
// bytes were committed.
func TestPutRecordsAfterCancel(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	inner := cancelAfterPut{BlobStore: f.mem, cancel: cancel}
	bs, err := pending.Wrap(f.pool, coretest.SingleStore{Store: inner}).ForRepository(context.Background(), f.repo)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := bs.Put(ctx, strings.NewReader("client left"))
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("context not cancelled")
	}
	if _, ok := f.row(t, d); !ok {
		t.Fatal("no row recorded after cancel")
	}
}

// cancelAfterPut cancels the request once the bytes are committed.
type cancelAfterPut struct {
	core.BlobStore
	cancel func()
}

func (c cancelAfterPut) Put(ctx context.Context, r io.Reader) (core.Digest, int64, error) {
	d, n, err := c.BlobStore.Put(ctx, r)
	c.cancel()
	return d, n, err
}

func TestFailedPutRecordsNothing(t *testing.T) {
	f := newFixture(t)
	boom := errors.New("boom")
	if _, _, err := f.bs.Put(context.Background(), io.MultiReader(strings.NewReader("partial"), errReader{boom})); !errors.Is(err, boom) {
		t.Fatalf("Put = %v, want %v", err, boom)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM blobs`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows = %d, %v; want none", n, err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestUnwrapKeepsTempStore(t *testing.T) {
	f := newFixture(t)
	if _, ok := core.TempStoreOf(f.bs); !ok {
		t.Fatal("temp store hidden by the wrapper")
	}
}
