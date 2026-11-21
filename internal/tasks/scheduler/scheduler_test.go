package scheduler_test

import (
	"context"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// Tickers run only on the leader, and move to the new leader on failover.
func TestTickersRunOnlyOnLeader(t *testing.T) {
	pool := pgtest.Pool(t)
	key := rand.Int64N(1<<40) + 1
	type node struct {
		ticks   atomic.Int32
		leader  atomic.Bool
		cancel  context.CancelFunc
		stopped chan struct{}
		mu      sync.Mutex
		at      []time.Time
	}
	start := func() *node {
		n := &node{stopped: make(chan struct{})}
		tick := scheduler.TickerFunc(func(_ context.Context, now time.Time) (time.Time, error) {
			if !n.leader.Load() {
				t.Error("ticker called on a follower")
			}
			n.mu.Lock()
			n.at = append(n.at, time.Now())
			n.mu.Unlock()
			n.ticks.Add(1)
			return now.Add(10 * time.Millisecond), nil // clamped to >= 1s
		})
		s := scheduler.New(pool, scheduler.Options{Tickers: []scheduler.Ticker{tick},
			RetryEvery: 50 * time.Millisecond, LockKey: key, OnLeader: n.leader.Store})
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
	// The ticker asks to run again after 10ms; the scheduler holds it to at
	// least a second. Measured between two real ticks, so a slow machine
	// cannot make this pass by accident.
	waitFor(t, "two leader ticks", func() bool { return a.ticks.Load() >= 2 })
	a.mu.Lock()
	gap := a.at[1].Sub(a.at[0])
	a.mu.Unlock()
	if gap < time.Second {
		t.Fatalf("second tick %v after the first; minimum gap ignored", gap)
	}
	if b.ticks.Load() != 0 {
		t.Fatalf("follower ticked %d times", b.ticks.Load())
	}
	once.Do(a.cancel)
	<-a.stopped
	waitFor(t, "failover", b.leader.Load)
	waitFor(t, "new leader ticks", func() bool { return b.ticks.Load() >= 1 })
}
