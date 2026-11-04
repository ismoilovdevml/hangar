// Package apitest holds helpers for integration tests of the REST API and the
// packages behind it (audit, search, catalog).
package apitest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/db"
)

// EnvTestDatabaseURL names the PostgreSQL used by integration tests.
const EnvTestDatabaseURL = "HANGAR_TEST_DATABASE_URL"

// MigratedPool connects to $HANGAR_TEST_DATABASE_URL inside a fresh schema,
// applies every migration and drops the schema when the test ends. The test
// is skipped when the variable is unset.
func MigratedPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv(EnvTestDatabaseURL)
	if base == "" {
		t.Skipf("%s not set; skipping database integration test", EnvTestDatabaseURL)
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, base, db.PoolOptions{MaxConns: 2})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "hangar_test_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := db.Open(ctx, u.String(), db.PoolOptions{MaxConns: 8})
	if err != nil {
		t.Fatalf("connect with schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close()
	})
	m, err := db.NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if err := m.Up(ctx, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// Exec runs sql on pool and fails the test on error.
func Exec(t testing.TB, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// QueryString runs a single-value query and returns the result as text.
func QueryString(t testing.TB, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return s
}
