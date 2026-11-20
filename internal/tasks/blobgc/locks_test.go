package blobgc

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// probeStore checks, while the collector deletes a blob's bytes, which blob
// rows are locked.
type probeStore struct {
	core.BlobStore
	t       *testing.T
	pool    *pgxpool.Pool
	storeID string
	all     []core.Digest
	calls   int
}

// locked reports whether another session holds the row lock of d.
func (s *probeStore) locked(ctx context.Context, d core.Digest) bool {
	s.t.Helper()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	_, err = tx.Exec(ctx, `SELECT 1 FROM blobs WHERE store_id = $1 AND digest = $2 FOR UPDATE NOWAIT`, s.storeID, string(d))
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "55P03" { // lock_not_available
		return true
	}
	if err != nil {
		s.t.Errorf("probe lock of %s: %v", d, err)
	}
	return false
}

func (s *probeStore) Delete(ctx context.Context, d core.Digest) error {
	s.calls++
	for _, other := range s.all {
		got := s.locked(ctx, other)
		if other == d && !got {
			s.t.Errorf("row of %s not locked while its bytes are deleted", d)
		}
		if other != d && got {
			s.t.Errorf("row of %s locked while the bytes of %s are deleted", other, d)
		}
	}
	return s.BlobStore.Delete(ctx, d)
}

// Deleting a blob's bytes holds that blob's row lock, and only that one: a
// slow store does not hold up publishes of the other blobs of the batch.
func TestCollectLocksOneRowAtATime(t *testing.T) {
	f := newFixture(t)
	var all []core.Digest
	for _, content := range []string{"one", "two", "three"} {
		d := f.put(content, 3*day)
		f.row(d, int64(len(content)), 0, 2*day, 3*day)
		all = append(all, d)
	}
	probe := &probeStore{BlobStore: f.store, t: t, pool: f.pool, storeID: f.storeID, all: all}
	c := New(f.pool, f.stores(probe), Options{GracePeriod: day, BatchSize: 10})
	res, err := c.Run(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Deleted != 3 || probe.calls != 3 || res.Stores[0].Batches != 1 {
		t.Fatalf("result = %+v, deletes = %d", res, probe.calls)
	}
	for _, d := range all {
		if f.hasRow(d) || f.hasBytes(d) {
			t.Errorf("%s not collected", d)
		}
	}
}
