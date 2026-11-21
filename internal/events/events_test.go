package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
)

// node opens a runtime on its own pool to the database (and schema) of
// pool, as a separate hangar process would.
func node(t *testing.T, pool *pgxpool.Pool, opts Options) *Runtime {
	t.Helper()
	p, err := pgxpool.NewWithConfig(context.Background(), pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	rt, err := Open(context.Background(), p, opts, nil)
	if err != nil {
		p.Close()
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { rt.Close(); p.Close() })
	return rt
}

// recorder collects the events a subscription receives.
type recorder struct {
	mu  sync.Mutex
	got []core.Event
}

func (r *recorder) handle(_ context.Context, e core.Event) {
	r.mu.Lock()
	r.got = append(r.got, e)
	r.mu.Unlock()
}

func (r *recorder) of(typ string) []core.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []core.Event
	for _, e := range r.got {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func subscribe(t *testing.T, bus core.EventBus, pattern string) *recorder {
	t.Helper()
	r := &recorder{}
	sub, err := bus.Subscribe(context.Background(), pattern, r.handle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	return r
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An event reaches the publishing node once (in process, not again through
// its own notification) and every other node once; patterns filter.
func TestBusAcrossNodes(t *testing.T) {
	pool := pgtest.Pool(t)
	a, b := node(t, pool, Options{}), node(t, pool, Options{})
	onA, onB := subscribe(t, a.Bus, "component.*"), subscribe(t, b.Bus, "component.*")
	allB := subscribe(t, b.Bus, ">")
	ctx := context.Background()

	if err := a.Bus.Publish(ctx, core.Event{Type: core.EventTaskProgress}); err != nil {
		t.Fatal(err)
	}
	if err := a.Bus.Publish(ctx, core.Event{Type: core.EventComponentPublished, Repository: "npm-hosted"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event on node b", func() bool { return len(onB.of(core.EventComponentPublished)) == 1 })
	eventually(t, "every event on node b", func() bool { return len(allB.of(core.EventTaskProgress)) == 1 })
	// Give a duplicate (a's own notification) time to arrive.
	time.Sleep(200 * time.Millisecond)
	for name, r := range map[string]*recorder{"a": onA, "b": onB} {
		got := r.of(core.EventComponentPublished)
		if len(got) != 1 || got[0].Repository != "npm-hosted" || got[0].ID == "" || got[0].At.IsZero() {
			t.Errorf("node %s received %+v, want the event once", name, got)
		}
		if n := len(r.of(core.EventTaskProgress)); n != 0 {
			t.Errorf("node %s: pattern component.* received %d task events", name, n)
		}
	}
	if err := a.Bus.Publish(ctx, core.Event{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("typeless event = %v", err)
	}
}

// Events above the NOTIFY payload limit travel through event_payloads.
func TestBusLargeEvent(t *testing.T) {
	pool := pgtest.Pool(t)
	a, b := node(t, pool, Options{}), node(t, pool, Options{})
	onB := subscribe(t, b.Bus, core.EventComponentPublished)
	big, _ := json.Marshal(map[string]string{"readme": strings.Repeat("x", 20000)})
	if err := a.Bus.Publish(context.Background(), core.Event{Type: core.EventComponentPublished, Data: big}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the large event", func() bool { return len(onB.of(core.EventComponentPublished)) == 1 })
	if got := onB.of(core.EventComponentPublished)[0].Data; string(got) != string(big) {
		t.Fatalf("payload of %d bytes arrived as %d bytes", len(big), len(got))
	}
}

// terminateListeners kills the listener sessions of the schema of pool, as
// a database restart or a network cut would.
func terminateListeners(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	ch, _ := Channels(schema)
	var n int
	err := pool.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
WHERE application_name = $1 AND query LIKE '%' || $2 || '%'`, ListenerApplicationName, strings.TrimPrefix(ch, EventChannel)).Scan(&n)
	if err != nil || n == 0 {
		t.Fatalf("terminated %d listeners: %v", n, err)
	}
}

// After the listener connection is lost and back, every subscription is
// told to resync (whatever its pattern), and events flow again.
func TestBusResyncAfterReconnect(t *testing.T) {
	pool := pgtest.Pool(t)
	a, b := node(t, pool, Options{}), node(t, pool, Options{})
	onB := subscribe(t, b.Bus, "repository.*")
	terminateListeners(t, pool)
	eventually(t, "resync on node b", func() bool { return len(onB.of(core.EventResync)) >= 1 })
	if b.Listener.Reconnects() == 0 {
		t.Fatal("reconnect not counted")
	}
	if err := b.Ping(context.Background()); err != nil {
		t.Fatalf("ping after reconnect: %v", err)
	}
	eventually(t, "node a listening again", a.Listener.Connected)
	if err := a.Bus.Publish(context.Background(), core.Event{Type: core.EventRepositoryUpdated, Repository: "r"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "events after the reconnect", func() bool { return len(onB.of(core.EventRepositoryUpdated)) == 1 })
}

// A subscriber that cannot keep up loses events and is told to resync.
func TestBusSlowSubscriberResyncs(t *testing.T) {
	pool := pgtest.Pool(t)
	a := node(t, pool, Options{})
	release := make(chan struct{})
	var once sync.Once
	r := &recorder{}
	sub, err := a.Bus.Subscribe(context.Background(), "task.*", func(ctx context.Context, e core.Event) {
		once.Do(func() { <-release })
		r.handle(ctx, e)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	for range subBuffer + 10 {
		a.Bus.dispatch(core.Event{Type: core.EventTaskProgress})
	}
	close(release)
	eventually(t, "a resync", func() bool { return len(r.of(core.EventResync)) == 1 })
	eventually(t, "the queued events", func() bool { return len(r.of(core.EventTaskProgress)) >= subBuffer })
}

func TestMatchSubject(t *testing.T) {
	for _, tc := range []struct {
		pattern, subject string
		want             bool
	}{
		{"component.*", "component.published", true},
		{"component.*", "component", false},
		{"component.*", "component.a.b", false},
		{">", "task.progress", true},
		{"repository.>", "repository.cache_invalidated", true},
		{"repository.>", "repository", false},
		{"task.progress", "task.progress", true},
		{"task.progress", "task.finished", false},
	} {
		if got := matchSubject(tc.pattern, tc.subject); got != tc.want {
			t.Errorf("matchSubject(%q, %q) = %v", tc.pattern, tc.subject, got)
		}
	}
}

func TestChannelNames(t *testing.T) {
	if e, j := Channels("public"); e != EventChannel || j != JobChannel {
		t.Fatalf("public = %s, %s", e, j)
	}
	e1, _ := Channels("tenant_a")
	e2, _ := Channels("tenant_b")
	if e1 == e2 || !strings.HasPrefix(e1, EventChannel+"_") || len(e1) > 63 {
		t.Fatalf("schema channels %q, %q", e1, e2)
	}
}
