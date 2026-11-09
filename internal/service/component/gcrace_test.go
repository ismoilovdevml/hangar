package component_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/service/component"
	"github.com/ismoilovdevml/hangar/internal/tasks/blobgc"
)

// The tests below drive blob GC into the publish path at the two points where
// the old code let an asset reference bytes that GC deleted. Both must end
// with the publish failing and no asset pointing at a missing blob.

const gcGrace = 24 * time.Hour

// eligibleBlob stores content and registers it as an unreferenced blob whose
// grace period has passed, so the next GC run deletes it.
func (f *fixture) eligibleBlob(t *testing.T, content string) (core.Digest, string) {
	t.Helper()
	d := f.blob(t, content)
	var storeID string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO blobs (store_id, digest, size, ref_count, soft_deleted_at)
		 SELECT id, $1, $2, 0, now() - interval '72 hours' FROM blob_stores WHERE name = 'default'
		 RETURNING store_id::text`, string(d), len(content)).Scan(&storeID); err != nil {
		t.Fatal(err)
	}
	return d, storeID
}

func (f *fixture) collector(pool *pgxpool.Pool, store core.BlobStore) *blobgc.Collector {
	return blobgc.New(pool, blobgc.StoresFunc(func(context.Context, string) (core.BlobStore, error) {
		return store, nil
	}), blobgc.Options{GracePeriod: gcGrace, TempMaxAge: -1})
}

// wantNoDanglingAssets fails if an asset references a digest the store lacks.
func (f *fixture) wantNoDanglingAssets(t *testing.T) {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT path, blob_digest FROM assets`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var path, d string
		if err := rows.Scan(&path, &d); err != nil {
			t.Fatal(err)
		}
		if _, err := f.blobs.Stat(context.Background(), core.Digest(d)); err != nil {
			t.Errorf("asset %s references %s, which is gone from the store: %v", path, d[:15], err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// GC runs to completion after Publish has seen the blob and before its
// transaction references it.
func TestPublishFailsWhenGCDeletesBlobAfterCheck(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-gc1", core.RepoHosted, core.WriteAllow)
	d, storeID := f.eligibleBlob(t, "layer bytes "+randHex(t))
	gc := f.collector(f.pool, f.blobs)

	ran := false
	component.SetAfterBlobCheck(f.svc, func() {
		ran = true
		res, err := gc.Run(context.Background(), blobgc.Request{StoreID: storeID})
		if err != nil || res.Deleted != 1 {
			t.Errorf("gc = %+v, %v; want 1 deleted", res, err)
		}
	})
	_, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Assets: []*core.Asset{asset("img/blobs/layer", d, "")}})
	if !ran {
		t.Fatal("hook did not run")
	}
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("publish of a collected blob = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.GetAsset(context.Background(), r.ID, "img/blobs/layer"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("asset after failed publish: %v, want ErrNotFound", err)
	}
	if row := f.blobRow(t, d); row.exists {
		t.Fatalf("blob row survived the failed publish: %+v", row)
	}
	f.wantNoDanglingAssets(t)
}

// gateStore blocks Delete of one digest until released, so a GC batch can be
// held with its candidate row locked and the bytes still present.
type gateStore struct {
	core.BlobStore
	digest   core.Digest
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
	released sync.Once
}

func (g *gateStore) Delete(ctx context.Context, d core.Digest) error {
	if d == g.digest {
		g.once.Do(func() { close(g.entered) })
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.BlobStore.Delete(ctx, d)
}

func (g *gateStore) open() { g.released.Do(func() { close(g.release) }) }

// GC has locked the blob row and is about to delete the bytes when Publish
// arrives: Publish sees the bytes, then waits on the row lock, and proceeds
// once GC has committed.
func TestPublishFailsWhenBlockedOnGCBatch(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-gc2", core.RepoHosted, core.WriteAllow)
	d, storeID := f.eligibleBlob(t, "layer bytes "+randHex(t))

	app := "gc-race-" + randHex(t)
	cfg := f.pool.Config()
	cfg.ConnConfig.RuntimeParams["application_name"] = app
	gcPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gcPool.Close)

	gate := &gateStore{BlobStore: f.blobs, digest: d, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(gate.open)
	gc := f.collector(gcPool, gate)
	gcDone := make(chan error, 1)
	go func() {
		_, err := gc.Run(context.Background(), blobgc.Request{StoreID: storeID})
		gcDone <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("gc did not reach the blob")
	}

	pubDone := make(chan error, 1)
	go func() {
		_, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Assets: []*core.Asset{asset("img/blobs/layer", d, "")}})
		pubDone <- err
	}()
	f.waitBlockedBy(t, app, pubDone)
	gate.open()

	if err := <-gcDone; err != nil {
		t.Fatalf("gc: %v", err)
	}
	if err := <-pubDone; !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("publish blocked on a collected blob = %v, want ErrNotFound", err)
	}
	if _, err := f.svc.GetAsset(context.Background(), r.ID, "img/blobs/layer"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("asset after failed publish: %v, want ErrNotFound", err)
	}
	f.wantNoDanglingAssets(t)
}

// waitBlockedBy waits until some session waits on a lock held by a session of
// application app.
func (f *fixture) waitBlockedBy(t *testing.T, app string, early <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := f.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity w
			 WHERE w.wait_event_type = 'Lock'
			   AND EXISTS (SELECT 1 FROM unnest(pg_blocking_pids(w.pid)) AS b(pid)
			                JOIN pg_stat_activity h ON h.pid = b.pid WHERE h.application_name = $1)`, app).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		select {
		case err := <-early:
			t.Fatalf("publish finished before blocking on the gc batch: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("publish never blocked on the gc batch")
}

// A blob that is unreferenced but inside its grace period is published
// normally and is not collected afterwards.
func TestPublishRevivesBlobInGracePeriod(t *testing.T) {
	f := newFixture(t)
	r := f.repo(t, "raw-gc3", core.RepoHosted, core.WriteAllow)
	content := "fresh " + randHex(t)
	d := f.blob(t, content)
	var storeID string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO blobs (store_id, digest, size, ref_count, soft_deleted_at)
		 SELECT id, $1, $2, 0, now() FROM blob_stores WHERE name = 'default' RETURNING store_id::text`,
		string(d), len(content)).Scan(&storeID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.Publish(f.ctx, r, core.PublishRequest{Assets: []*core.Asset{asset("a/b", d, "")}}); err != nil {
		t.Fatal(err)
	}
	f.wantBlob(t, d, 1, false)
	if res, err := f.collector(f.pool, f.blobs).Run(context.Background(), blobgc.Request{StoreID: storeID}); err != nil || res.Deleted != 0 {
		t.Fatalf("gc = %+v, %v; want nothing deleted", res, err)
	}
	f.wantNoDanglingAssets(t)
}

func randHex(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(hex.EncodeToString(b[:]))
}
