package component_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/service/component"
)

// lockComponent opens a transaction holding the component row lock, as a
// delete does before it touches the component's assets, and returns it
// with its backend pid.
func lockComponent(t *testing.T, pool *pgxpool.Pool, id string) (pgx.Tx, int32) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM components WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	return tx, pid
}

// waitBlockedBy waits until some session waits for a lock held by pid.
func waitBlockedBy(t *testing.T, pool *pgxpool.Pool, pid int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no session waits for the lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A flush running while a delete holds the component and then locks its
// asset must wait for the delete, not deadlock with it: both lock the
// component first.
func TestFlushDownloadsLockOrder(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	c, as, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "lib", "1", nil),
		Assets: []*core.Asset{asset("lib/1/a", f.blob(t, "a"), "")}})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.RecordDownload(f.ctx, as[0])

	tx, pid := lockComponent(t, f.pool, c.ID)
	flushed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		flushed <- f.svc.FlushDownloads(ctx)
	}()
	waitBlockedBy(t, f.pool, pid)

	ctx := context.Background()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM assets WHERE id = $1 FOR UPDATE`, as[0].ID); err != nil {
		t.Fatalf("delete locking the asset after its component: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-flushed; err != nil {
		t.Fatalf("FlushDownloads: %v", err)
	}
	a, _ := f.svc.GetAsset(f.ctx, r.ID, "lib/1/a")
	got, _ := f.svc.GetComponentByID(f.ctx, c.ID)
	if a.DownloadCount != 1 || got.DownloadCount != 1 {
		t.Fatalf("counts: asset=%d component=%d, want 1 and 1", a.DownloadCount, got.DownloadCount)
	}
}

// While flushes fail the buffered counts are kept for the next flush, but
// at most MaxBuffered assets: downloads of further assets are dropped.
func TestRecordDownloadBufferBounded(t *testing.T) {
	f := newFixture(t, func(o *component.Options) { o.MaxBuffered = 2 })
	r := f.repo(t, "raw-h", core.RepoHosted, core.WriteAllow)
	c, as, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Component: comp("", "lib", "1", nil),
		Assets: []*core.Asset{asset("lib/1/a", f.blob(t, "a"), ""), asset("lib/1/b", f.blob(t, "b"), ""),
			asset("lib/1/c", f.blob(t, "c"), "")}})
	if err != nil {
		t.Fatal(err)
	}
	f.svc.RecordDownload(f.ctx, as[0])
	f.svc.RecordDownload(f.ctx, as[1])

	// The database does not answer in time: the flush fails.
	tx, _ := lockComponent(t, f.pool, c.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := f.svc.FlushDownloads(ctx); err == nil {
		t.Fatal("flush succeeded against a locked component")
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.svc.RecordDownload(f.ctx, as[0]) // buffered asset: counted
	f.svc.RecordDownload(f.ctx, as[2]) // buffer full: dropped
	if err := f.svc.FlushDownloads(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"lib/1/a": 2, "lib/1/b": 1, "lib/1/c": 0}
	for path, n := range want {
		a, err := f.svc.GetAsset(f.ctx, r.ID, path)
		if err != nil {
			t.Fatal(err)
		}
		if a.DownloadCount != n {
			t.Errorf("%s download count = %d, want %d", path, a.DownloadCount, n)
		}
	}
	if got, _ := f.svc.GetComponentByID(f.ctx, c.ID); got.DownloadCount != 3 {
		t.Errorf("component download count = %d, want 3", got.DownloadCount)
	}
}
