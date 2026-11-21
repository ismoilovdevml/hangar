package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
)

// fast are queue options that keep tests short.
func fast(name string) Options {
	return Options{Queue: QueueOptions{Node: name, PollInterval: 100 * time.Millisecond, RetryBase: 50 * time.Millisecond,
		ShutdownWait: 5 * time.Second}}
}

func jobRows(t *testing.T, pool *pgxpool.Pool, typ string) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT state, count(*) FROM jobs WHERE type = $1 GROUP BY state`, typ)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func TestQueueRetriesThenSucceeds(t *testing.T) {
	pool := pgtest.Pool(t)
	rt := node(t, pool, fast("n1"))
	var calls atomic.Int32
	var attempts []int
	var mu sync.Mutex
	if err := rt.Queue.Handle("test.flaky", func(_ context.Context, j core.Job) error {
		mu.Lock()
		attempts = append(attempts, j.Attempt)
		mu.Unlock()
		if calls.Add(1) == 1 {
			return errors.New("transient")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Queue.Enqueue(context.Background(), core.Job{Type: "test.flaky", Payload: json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the job to finish", func() bool { return len(jobRows(t, pool, "test.flaky")) == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("attempts = %v, want [1 2]", attempts)
	}
}

func TestQueueDeadAfterMaxAttempts(t *testing.T) {
	pool := pgtest.Pool(t)
	opts := fast("n1")
	opts.Queue.MaxAttempts = 3
	rt := node(t, pool, opts)
	var calls atomic.Int32
	if err := rt.Queue.Handle("test.broken", func(context.Context, core.Job) error {
		calls.Add(1)
		return errors.New("always")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Queue.Enqueue(context.Background(), core.Job{Type: "test.broken"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a dead job", func() bool { return jobRows(t, pool, "test.broken")["dead"] == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 3 {
		t.Fatalf("ran %d times, want 3", n)
	}
	counts, err := rt.Queue.Counts(context.Background())
	if err != nil || counts["test.broken"].Dead != 1 {
		t.Fatalf("counts = %+v, %v", counts, err)
	}
}

// A key stores a job once while it waits and within the dedup window after.
func TestQueueDedupByKey(t *testing.T) {
	pool := pgtest.Pool(t)
	rt := node(t, pool, fast("n1"))
	ctx := context.Background()
	id1, err := rt.Queue.Enqueue(ctx, core.Job{Type: "test.dedup", Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := rt.Queue.Enqueue(ctx, core.Job{Type: "test.dedup", Key: "k"})
	if err != nil || id2 != id1 {
		t.Fatalf("second enqueue = %q, %v; want %q", id2, err, id1)
	}
	var calls atomic.Int32
	if err := rt.Queue.Handle("test.dedup", func(context.Context, core.Job) error { calls.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the job to run", func() bool { return jobRows(t, pool, "test.dedup")["done"] == 1 })
	if _, err := rt.Queue.Enqueue(ctx, core.Job{Type: "test.dedup", Key: "k"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("ran %d times, want 1 (deduplicated within the window)", n)
	}
}

func TestQueueRejectsInvalidJobs(t *testing.T) {
	pool := pgtest.Pool(t)
	rt := node(t, pool, fast("n1"))
	for name, j := range map[string]core.Job{
		"no type":            {},
		"wildcard type":      {Type: "scan.*"},
		"payload not JSON":   {Type: "test.x", Payload: json.RawMessage("{")},
		"debounce, no key":   {Type: "test.x", Debounce: time.Second},
		"type with a space":  {Type: "test x"},
		"type ending in dot": {Type: "test."},
	} {
		if _, err := rt.Queue.Enqueue(context.Background(), j); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s: %v, want invalid", name, err)
		}
	}
	if err := rt.Queue.Handle("test.x", func(context.Context, core.Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := rt.Queue.Handle("test.x", func(context.Context, core.Job) error { return nil }); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("second handler = %v", err)
	}
}

// Two nodes share the queue: every job runs exactly once, and a node runs
// one job at a time.
func TestQueueClaimExclusive(t *testing.T) {
	pool := pgtest.Pool(t)
	const jobs = 200
	var (
		mu   sync.Mutex
		runs = map[string]int{}
	)
	nodes := []*Runtime{node(t, pool, fast("a")), node(t, pool, fast("b"))}
	perNode := make([]atomic.Int32, len(nodes))
	for i, rt := range nodes {
		var busy atomic.Int32
		if err := rt.Queue.Handle("test.exclusive", func(_ context.Context, j core.Job) error {
			if busy.Add(1) != 1 {
				t.Errorf("node %d runs two jobs at once", i)
			}
			defer busy.Add(-1)
			time.Sleep(time.Millisecond)
			mu.Lock()
			runs[string(j.Payload)]++
			mu.Unlock()
			perNode[i].Add(1)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range jobs {
		if _, err := nodes[i%2].Queue.Enqueue(context.Background(), core.Job{Type: "test.exclusive", Payload: json.RawMessage(fmt.Sprint(i))}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "every job", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(runs) == jobs
	})
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for p, n := range runs {
		if n != 1 {
			t.Errorf("job %s ran %d times", p, n)
		}
	}
	if perNode[0].Load() == 0 || perNode[1].Load() == 0 {
		t.Errorf("work not shared: %d and %d", perNode[0].Load(), perNode[1].Load())
	}
}

// A job that runs much longer than the lease keeps it through heartbeats:
// the other node never takes it over.
func TestQueueHeartbeatKeepsLongJob(t *testing.T) {
	pool := pgtest.Pool(t)
	opts := fast("a")
	opts.Queue.Visibility = 600 * time.Millisecond
	opts.Queue.Timeout = time.Minute
	var calls atomic.Int32
	for _, name := range []string{"a", "b"} {
		opts.Queue.Node = name
		rt := node(t, pool, opts)
		if err := rt.Queue.Handle("test.long", func(ctx context.Context, _ core.Job) error {
			calls.Add(1)
			select {
			case <-time.After(5 * opts.Queue.Visibility):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}); err != nil {
			t.Fatal(err)
		}
		if name == "a" {
			if _, err := rt.Queue.Enqueue(context.Background(), core.Job{Type: "test.long"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	eventually(t, "the long job to finish", func() bool { return calls.Load() >= 1 && len(jobRows(t, pool, "test.long")) == 0 })
	if n := calls.Load(); n != 1 {
		t.Fatalf("long job ran %d times, want 1", n)
	}
}

// The job of a node that died (its lease expired) is run by another node;
// a live lease is respected.
func TestQueueReclaimsExpiredLease(t *testing.T) {
	pool := pgtest.Pool(t)
	ctx := context.Background()
	rt := node(t, pool, fast("survivor"))
	for id, until := range map[string]string{"crashed": "now() - interval '1 second'", "alive": "now() + interval '1 hour'"} {
		if _, err := pool.Exec(ctx, `INSERT INTO jobs (id, type, state, attempts, locked_by, locked_until, started_at)
VALUES ($1, 'test.crash', 'running', 1, 'gone/1', `+until+`, now())`, id); err != nil {
			t.Fatal(err)
		}
	}
	got := make(chan core.Job, 2)
	if err := rt.Queue.Handle("test.crash", func(_ context.Context, j core.Job) error { got <- j; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case j := <-got:
		if j.ID != "crashed" || j.Attempt != 2 {
			t.Fatalf("reclaimed %+v, want job crashed at attempt 2", j)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("expired job not reclaimed")
	}
	select {
	case j := <-got:
		t.Fatalf("job %s with a live lease was taken", j.ID)
	case <-time.After(500 * time.Millisecond):
	}
}

// Many enqueues of a debounced key while it waits run it once; one enqueued
// while it runs runs again afterwards.
func TestQueueCoalescing(t *testing.T) {
	pool := pgtest.Pool(t)
	a, b := node(t, pool, fast("a")), node(t, pool, fast("b"))
	ctx := context.Background()
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	var calls atomic.Int32
	for _, rt := range []*Runtime{a, b} {
		if err := rt.Queue.Handle("test.reindex", func(context.Context, core.Job) error {
			if calls.Add(1) == 1 {
				started <- struct{}{}
				<-release
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	job := core.Job{Type: "test.reindex", Key: "repo-1", Debounce: 2 * time.Second}
	var wg sync.WaitGroup
	ids := make(chan string, 1000)
	for i := range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := []*Runtime{a, b}[i%2].Queue.Enqueue(ctx, job)
			if err != nil {
				t.Error(err)
			}
			ids <- id
		}()
	}
	wg.Wait()
	close(ids)
	distinct := map[string]bool{}
	for id := range ids {
		distinct[id] = true
	}
	if len(distinct) != 1 {
		t.Fatalf("1000 enqueues made %d jobs", len(distinct))
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("coalesced job did not run")
	}
	// Changed while it runs: a second run follows.
	if _, err := a.Queue.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond) // past the debounce: the second may not start while the first runs
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d runs while the first was running", n)
	}
	close(release)
	eventually(t, "the follow-up run", func() bool { return calls.Load() == 2 && len(jobRows(t, pool, "test.reindex")) == 0 })
	time.Sleep(500 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("ran %d times, want 2", n)
	}
}

// Close cancels running handlers, waits for them, and hands their jobs
// back to the queue without counting the attempt.
func TestQueueCloseWaitsAndReturnsJob(t *testing.T) {
	pool := pgtest.Pool(t)
	rt := node(t, pool, fast("a"))
	running := make(chan struct{})
	var returned atomic.Bool
	if err := rt.Queue.Handle("test.interrupt", func(ctx context.Context, _ core.Job) error {
		close(running)
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond) // cleanup that Close must wait for
		returned.Store(true)
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Queue.Enqueue(context.Background(), core.Job{Type: "test.interrupt"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-running:
	case <-time.After(10 * time.Second):
		t.Fatal("job did not start")
	}
	rt.Queue.Close()
	if !returned.Load() {
		t.Fatal("Close returned before the handler")
	}
	var state string
	var attempts int
	if err := pool.QueryRow(context.Background(), `SELECT state, attempts FROM jobs WHERE type = 'test.interrupt'`).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "queued" || attempts != 0 {
		t.Fatalf("interrupted job is %s after %d attempts, want queued after 0", state, attempts)
	}
	if err := rt.Queue.Handle("test.after", func(context.Context, core.Job) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Handle after Close = %v", err)
	}
	// Another node picks it up.
	b := node(t, pool, fast("b"))
	done := make(chan struct{})
	if err := b.Queue.Handle("test.interrupt", func(context.Context, core.Job) error { close(done); return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("returned job not run by another node")
	}
}

func TestRetryDelay(t *testing.T) {
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{{1, 5 * time.Second}, {2, 10 * time.Second}, {3, 20 * time.Second}, {20, 10 * time.Minute}} {
		if got := retryDelay(5*time.Second, tc.attempt); got != tc.want {
			t.Errorf("retryDelay(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}
