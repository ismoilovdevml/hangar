package events

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func openEmbedded(t *testing.T) *Runtime {
	t.Helper()
	rt, err := Open(context.Background(), Options{Embedded: true, DataDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(rt.Close)
	return rt
}

func TestBusPublishSubscribe(t *testing.T) {
	rt := openEmbedded(t)
	ctx := context.Background()
	if err := rt.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	bus := NewBus(rt.Conn, nil)
	got := make(chan core.Event, 4)
	sub, err := bus.Subscribe(ctx, "component.*", func(_ context.Context, e core.Event) { got <- e })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	if err := bus.Publish(ctx, core.Event{Type: core.EventTaskProgress}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(ctx, core.Event{Type: core.EventComponentPublished, Repository: "npm-hosted"}); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		if e.Type != core.EventComponentPublished || e.Repository != "npm-hosted" || e.ID == "" || e.At.IsZero() {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event received")
	}
	select {
	case e := <-got:
		t.Fatalf("unexpected event %s (pattern should not match)", e.Type)
	case <-time.After(100 * time.Millisecond):
	}
	if err := bus.Publish(ctx, core.Event{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("typeless event = %v", err)
	}
}

func TestQueueRunsRetriesAndDedups(t *testing.T) {
	rt := openEmbedded(t)
	ctx := context.Background()
	q, err := NewQueue(ctx, rt.JS, QueueOptions{MaxAttempts: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)

	var calls atomic.Int32
	done := make(chan core.Job, 1)
	if err := q.Handle("test.flaky", func(_ context.Context, j core.Job) error {
		if calls.Add(1) == 1 {
			return errors.New("transient")
		}
		done <- j
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.Handle("test.flaky", func(context.Context, core.Job) error { return nil }); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("double Handle = %v", err)
	}

	id, err := q.Enqueue(ctx, core.Job{Type: "test.flaky", Key: "k1", Payload: []byte(`{"n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, core.Job{Type: "test.flaky", Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	// Deduplication happens when the job is stored: the second Enqueue with
	// the same key adds nothing to the stream.
	info, err := q.stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.LastSeq != 1 {
		t.Fatalf("stream holds %d jobs after two enqueues with one key, want 1", info.State.LastSeq)
	}
	select {
	case j := <-done:
		if j.ID != id || j.Attempt != 2 || string(j.Payload) != `{"n":1}` {
			t.Fatalf("job = %+v", j)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("job not retried; calls=%d", calls.Load())
	}
	// Once the stream is empty nothing is left to deliver, so the call count
	// is final: one failure plus one retry.
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := q.stream.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.State.Msgs == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still in the stream after it succeeded: %+v", info.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("handler calls = %d, want 2 (dedup by key)", n)
	}
	if _, err := q.Enqueue(ctx, core.Job{Type: "bad type"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad type = %v", err)
	}
}

func TestRetryDelay(t *testing.T) {
	if retryDelay(1) != 5*time.Second || retryDelay(2) != 10*time.Second || retryDelay(30) != 10*time.Minute {
		t.Fatal("retryDelay schedule changed")
	}
}
