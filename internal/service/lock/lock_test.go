package lock_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/service/lock"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
)

// exclusive runs workers that each take key on one of lockers and checks
// that no two ever hold it at once.
func exclusive(t *testing.T, lockers []core.Locker) {
	t.Helper()
	var (
		inside atomic.Int32
		max    atomic.Int32
		wg     sync.WaitGroup
	)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := lockers[i%len(lockers)]
			for range 5 {
				unlock, err := l.Lock(context.Background(), "npm/repo/pkg")
				if err != nil {
					t.Error(err)
					return
				}
				n := inside.Add(1)
				if n > max.Load() {
					max.Store(n)
				}
				time.Sleep(200 * time.Microsecond)
				inside.Add(-1)
				unlock()
			}
		}()
	}
	wg.Wait()
	if got := max.Load(); got != 1 {
		t.Fatalf("max concurrent holders = %d, want 1", got)
	}
}

func newLocker(t *testing.T, pool *pgxpool.Pool, maxHeld int) *lock.Locker {
	t.Helper()
	l, err := lock.New(context.Background(), pool, lock.Options{MaxHeld: maxHeld})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l
}

func TestLocalExclusive(t *testing.T) {
	exclusive(t, []core.Locker{lock.NewLocal()})
}

func TestAdvisoryExclusiveAcrossNodes(t *testing.T) {
	pool := pgtest.Pool(t)
	// Two Lockers model two nodes: only PostgreSQL can make them agree.
	exclusive(t, []core.Locker{newLocker(t, pool, 0), newLocker(t, pool, 2)})
}

func TestAdvisoryCancelWhileWaiting(t *testing.T) {
	pool := pgtest.Pool(t)
	a, b := newLocker(t, pool, 0), newLocker(t, pool, 0)
	unlock, err := a.Lock(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := b.Lock(ctx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting Lock = %v, want deadline exceeded", err)
	}
	// Other keys are independent.
	u2, err := b.Lock(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	u2()
	unlock()
	unlock() // idempotent
	u3, err := b.Lock(context.Background(), "k")
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	u3()
	// No advisory lock may be left behind (the cancelled wait's session was closed).
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid <> pg_backend_pid() AND granted
		   AND (classid::bigint << 32 | objid::bigint) = ANY($1)`,
		[]int64{lock.ID("k"), lock.ID("other")}).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d advisory locks still held", n)
	}
}

func TestLocalCancel(t *testing.T) {
	l := lock.NewLocal()
	unlock, _ := l.Lock(context.Background(), "k")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Lock(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Lock = %v, want canceled", err)
	}
	unlock()
	u, err := l.Lock(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	u()
}
