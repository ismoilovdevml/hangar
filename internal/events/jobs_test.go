package events

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// waitEmpty waits until the job stream holds no message: every job was
// acknowledged (or terminated).
func waitEmpty(t *testing.T, q *Queue) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := q.stream.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.State.Msgs == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs left in the stream: %+v", info.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A job that runs longer than AckWait is not redelivered while its node is
// still running it.
func TestQueueLongJobNotRedelivered(t *testing.T) {
	rt := openEmbedded(t)
	ctx := context.Background()
	const ackWait = time.Second
	q, err := NewQueue(ctx, rt.JS, QueueOptions{AckWait: ackWait, Timeout: time.Minute}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	var calls atomic.Int32
	if err := q.Handle("test.long", func(ctx context.Context, _ core.Job) error {
		calls.Add(1)
		select {
		case <-time.After(3 * ackWait):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, core.Job{Type: "test.long"}); err != nil {
		t.Fatal(err)
	}
	waitEmpty(t, q)
	time.Sleep(2 * ackWait) // a redelivery would have arrived by now
	if n := calls.Load(); n != 1 {
		t.Fatalf("handler calls = %d, want 1", n)
	}
}

// Close cancels the running handlers and returns only after they have; the
// interrupted job goes back to the queue for another node.
func TestQueueCloseWaitsForHandlers(t *testing.T) {
	rt := openEmbedded(t)
	ctx := context.Background()
	q, err := NewQueue(ctx, rt.JS, QueueOptions{ShutdownWait: 10 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var finished atomic.Bool
	if err := q.Handle("test.close", func(ctx context.Context, _ core.Job) error {
		close(started)
		<-ctx.Done()
		time.Sleep(200 * time.Millisecond) // cleanup that must not outlive Close
		finished.Store(true)
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	id, err := q.Enqueue(ctx, core.Job{Type: "test.close"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("job not started")
	}
	start := time.Now()
	q.Close()
	if !finished.Load() {
		t.Fatal("Close returned while a handler was still running")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Close took %s", took)
	}

	// Another node picks the interrupted job up at once.
	q2, err := NewQueue(ctx, rt.JS, QueueOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q2.Close)
	got := make(chan core.Job, 1)
	if err := q2.Handle("test.close", func(_ context.Context, j core.Job) error { got <- j; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case j := <-got:
		if j.ID != id {
			t.Fatalf("redelivered job = %+v", j)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interrupted job not redelivered")
	}
}

// A node holds only the job it is running: queued jobs go to idle nodes
// instead of waiting behind a long one.
func TestQueueSpreadsJobsAcrossNodes(t *testing.T) {
	rt := openEmbedded(t)
	ctx := context.Background()
	release := make(chan struct{})
	running := make(chan string, 4)
	handler := func(node string) core.JobHandler {
		return func(ctx context.Context, _ core.Job) error {
			running <- node
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	defer close(release)
	// A backlog is waiting when the nodes start: the first one to fetch must
	// not take all of it.
	q, err := NewQueue(ctx, rt.JS, QueueOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := q.Enqueue(ctx, core.Job{Type: "test.spread"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range []string{"a", "b"} {
		q, err := NewQueue(ctx, rt.JS, QueueOptions{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(q.Close)
		if err := q.Handle("test.spread", handler(node)); err != nil {
			t.Fatal(err)
		}
		if node == "a" {
			select {
			case n := <-running:
				running <- n // node a fetched and runs a job
			case <-time.After(10 * time.Second):
				t.Fatal("no job started on node a")
			}
		}
	}
	nodes := map[string]bool{}
	for range 2 {
		select {
		case n := <-running:
			nodes[n] = true
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of 2 jobs running (nodes %v): one node holds the other", len(nodes), nodes)
		}
	}
	if len(nodes) != 2 {
		t.Fatalf("jobs ran on %v, want one on each node", nodes)
	}
}
