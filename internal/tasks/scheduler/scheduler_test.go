package scheduler_test

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
	"github.com/ismoilovdevml/hangar/internal/tasks/scheduler"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Only one node enqueues; when it stops, another takes over.
func TestLeaderElection(t *testing.T) {
	pool := pgtest.Pool(t)
	key := rand.Int64N(1<<40) + 1 // advisory locks are database-wide
	entries := []scheduler.Entry{{Name: "tick", Every: 50 * time.Millisecond, Job: core.Job{Type: "test.tick"}}}

	type node struct {
		q       *coretest.JobQueue
		leader  atomic.Bool
		cancel  context.CancelFunc
		stopped chan struct{}
	}
	start := func() *node {
		n := &node{q: coretest.NewJobQueue(), stopped: make(chan struct{})}
		s, err := scheduler.New(pool, n.q, scheduler.Options{Entries: entries, RetryEvery: 50 * time.Millisecond,
			LockKey: key, OnLeader: n.leader.Store})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		n.cancel = cancel
		go func() { s.Run(ctx); close(n.stopped) }()
		return n
	}
	var once sync.Once
	a := start()
	waitFor(t, "first leader", a.leader.Load)
	b := start()
	t.Cleanup(func() { once.Do(a.cancel); b.cancel(); <-b.stopped })
	waitFor(t, "leader enqueues", func() bool { return len(a.q.Jobs()) >= 3 })
	if b.leader.Load() || len(b.q.Jobs()) != 0 {
		t.Fatalf("follower acted as leader: %d jobs", len(b.q.Jobs()))
	}
	once.Do(a.cancel)
	<-a.stopped
	waitFor(t, "failover", b.leader.Load)
	waitFor(t, "new leader enqueues", func() bool { return len(b.q.Jobs()) >= 1 })
	if j := b.q.Jobs()[0]; j.Type != "test.tick" || j.Key == "" {
		t.Errorf("job = %+v", j)
	}
}

func TestEntryValidation(t *testing.T) {
	if _, err := scheduler.New(nil, coretest.NewJobQueue(), scheduler.Options{Entries: []scheduler.Entry{{Name: "x"}}}); err == nil {
		t.Fatal("entry without interval and type accepted")
	}
}

// Tickers run only on the leader, and move to the new leader on failover.
func TestTickersRunOnlyOnLeader(t *testing.T) {
	pool := pgtest.Pool(t)
	key := rand.Int64N(1<<40) + 1
	type node struct {
		ticks   atomic.Int32
		leader  atomic.Bool
		cancel  context.CancelFunc
		stopped chan struct{}
	}
	start := func() *node {
		n := &node{stopped: make(chan struct{})}
		tick := scheduler.TickerFunc(func(_ context.Context, now time.Time) (time.Time, error) {
			if !n.leader.Load() {
				t.Error("ticker called on a follower")
			}
			n.ticks.Add(1)
			return now.Add(10 * time.Millisecond), nil // clamped to >= 1s
		})
		s, err := scheduler.New(pool, coretest.NewJobQueue(), scheduler.Options{Tickers: []scheduler.Ticker{tick},
			RetryEvery: 50 * time.Millisecond, LockKey: key, OnLeader: n.leader.Store})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		n.cancel = cancel
		go func() { s.Run(ctx); close(n.stopped) }()
		return n
	}
	a := start()
	waitFor(t, "first leader", a.leader.Load)
	b := start()
	var once sync.Once
	t.Cleanup(func() { once.Do(a.cancel); b.cancel(); <-b.stopped })
	waitFor(t, "leader ticks", func() bool { return a.ticks.Load() >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := a.ticks.Load(); n > 2 {
		t.Fatalf("ticker ran %d times in ~0.3s; minimum gap ignored", n)
	}
	if b.ticks.Load() != 0 {
		t.Fatalf("follower ticked %d times", b.ticks.Load())
	}
	once.Do(a.cancel)
	<-a.stopped
	waitFor(t, "failover", b.leader.Load)
	waitFor(t, "new leader ticks", func() bool { return b.ticks.Load() >= 1 })
}
