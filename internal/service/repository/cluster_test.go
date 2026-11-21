package repository_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/events"
	"github.com/ismoilovdevml/hangar/internal/service/repository"
)

// apiNode is one hangar process: its own pool, event bus and repository
// service with the cache watching the bus, as cmd/hangar wires them.
type apiNode struct {
	svc *repository.Service
	rt  *events.Runtime
}

func startAPINode(t *testing.T, f *fixture) *apiNode {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, f.pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	rt, err := events.Open(ctx, pool, events.Options{}, nil)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	svc := repository.New(pool, repository.Options{Formats: formats, AuthZ: f.authz, Audit: f.audit, Events: rt.Bus})
	sub, err := svc.Watch(ctx, rt.Bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe(); rt.Close(); pool.Close() })
	return &apiNode{svc: svc, rt: rt}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A change made through one node invalidates the cache of another node
// sharing the database (LISTEN/NOTIFY); after the other node's listener
// connection is lost and back, its cache starts over, so a change it may
// have missed shows.
func TestCacheInvalidatedAcrossNodes(t *testing.T) {
	f := newFixture(t)
	a, b := startAPINode(t, f), startAPINode(t, f)
	ctx := coretest.AsUser(context.Background(), "admin")
	if _, err := a.svc.Create(ctx, hosted("raw-a", "raw")); err != nil {
		t.Fatal(err)
	}
	description := func(n *apiNode) string {
		r, err := n.svc.Get(ctx, "raw-a")
		if err != nil {
			return "error: " + err.Error()
		}
		return r.Description
	}
	waitUntil(t, "node b to see the repository", func() bool { return description(b) == "" })

	up := hosted("raw-a", "raw")
	up.Description = "changed on a"
	if _, err := a.svc.Update(ctx, up); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "node b's cache to drop the old repository", func() bool { return description(b) == "changed on a" })

	// A change no event announces (another node's notification lost while
	// b was disconnected) shows once b's listener is back.
	f.setDescription(t, "raw-a", "missed")
	if got := description(b); got != "changed on a" {
		t.Fatalf("b read the database without an event: %q", got)
	}
	var schema string
	if err := f.pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	ch, _ := events.Channels(schema)
	if _, err := f.pool.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
WHERE application_name = $1 AND query LIKE '%' || $2 || '%'`, events.ListenerApplicationName, strings.TrimPrefix(ch, events.EventChannel)); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "node b to resync", func() bool { return description(b) == "missed" })
}
