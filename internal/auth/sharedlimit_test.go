package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/auth/authtest"
)

// The login rate limit is a guard against password guessing: two nodes
// behind one load balancer must allow what one node allows, not twice as
// much. Before, each node kept its own buckets in memory.
func TestLoginRateIsClusterWide(t *testing.T) {
	cfg := auth.Config{LoginRate: auth.RateLimit{PerMinute: 6, Burst: 3}}
	a := authtest.New(t, cfg)
	b := authtest.NewOnPool(t, a.Pool, cfg, a.Clock) // a second node: same database and clock
	client := from("198.51.100.23")

	steps := []struct {
		name string
		node *authtest.Env
		ok   bool
	}{
		{"node a, 1st", a, true},
		{"node b, 2nd", b, true},
		{"node a, 3rd", a, true},
		{"node b, over the burst", b, false},
		{"node a, over the burst", a, false},
	}
	for _, s := range steps {
		ok, wait := s.node.Svc.AllowLogin(client)
		if ok != s.ok {
			t.Fatalf("%s: allowed = %v, want %v", s.name, ok, s.ok)
		}
		if !ok && (wait <= 0 || wait > 10*time.Second) {
			t.Fatalf("%s: wait %s, want up to the 10 s one token takes", s.name, wait)
		}
	}
	// Another address has its own bucket.
	if ok, _ := b.Svc.AllowLogin(from("203.0.113.5")); !ok {
		t.Fatal("another address was limited")
	}
	// One token every 10 s.
	a.Clock.Advance(10 * time.Second)
	if ok, _ := b.Svc.AllowLogin(client); !ok {
		t.Fatal("no token after the refill time")
	}
	if ok, _ := a.Svc.AllowLogin(client); ok {
		t.Fatal("two tokens after one refill time")
	}

	// Full buckets are purged; half-empty ones are kept.
	a.Clock.Advance(time.Minute)
	if ok, _ := a.Svc.AllowLogin(from("192.0.2.99")); !ok {
		t.Fatal("fresh address limited")
	}
	if err := auth.PurgeBuckets(context.Background(), a.Pool, a.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	var keys []string
	rows, err := a.Pool.Query(context.Background(), `SELECT key FROM rate_limit_buckets ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	if keys, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "192.0.2.99" {
		t.Fatalf("buckets after purge = %v, want only the one still refilling", keys)
	}
}

// failingStore is a database that cannot be reached.
type failingStore struct{}

var errDown = errors.New("connection refused")

func (failingStore) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{} }

func (failingStore) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errDown
}

type errRow struct{}

func (errRow) Scan(...any) error { return errDown }

// Without its database the limiter still limits, per node.
func TestSharedLimiterFallsBackToMemory(t *testing.T) {
	clock := authtest.NewClock()
	l := auth.NewSharedLimiter(failingStore{}, auth.ScopeLogin, auth.RateLimit{PerMinute: 60, Burst: 2}, clock.Now, nil)
	for i, want := range []bool{true, true, false} {
		if ok, _, wait := l.Take(context.Background(), "k"); ok != want || (!ok && wait <= 0) {
			t.Fatalf("take %d = %v (wait %s), want %v", i, ok, wait, want)
		}
	}
	if auth.NewSharedLimiter(failingStore{}, auth.ScopeLogin, auth.RateLimit{}, nil, nil) != nil {
		t.Fatal("a disabled limit must be nil")
	}
	var disabled *auth.SharedLimiter
	if ok, _, _ := disabled.Take(context.Background(), "k"); !ok {
		t.Fatal("a nil limiter must allow")
	}
}
