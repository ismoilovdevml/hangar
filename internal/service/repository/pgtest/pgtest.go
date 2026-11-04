// Package pgtest provides PostgreSQL fixtures for service integration tests:
// a migrated database in a throwaway schema, and helpers to seed rows.
package pgtest

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

// EnvDatabaseURL names the PostgreSQL used by integration tests. Tests that
// need a database are skipped when it is unset.
const EnvDatabaseURL = "HANGAR_TEST_DATABASE_URL"

// Pool returns a pool connected to a fresh schema with every migration
// applied. The schema is dropped when the test ends. The test is skipped when
// HANGAR_TEST_DATABASE_URL is unset.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv(EnvDatabaseURL)
	if base == "" {
		t.Skipf("%s not set; skipping database integration test", EnvDatabaseURL)
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
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := db.Open(ctx, u.String(), db.PoolOptions{MaxConns: 16})
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

// AddBlobStore inserts a blob_stores row and returns its id. config may be nil.
func AddBlobStore(t testing.TB, pool *pgxpool.Pool, name, kind string, config map[string]any) string {
	t.Helper()
	if config == nil {
		config = map[string]any{}
	}
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO blob_stores (name, kind, config) VALUES ($1, $2, $3) RETURNING id::text`,
		name, kind, config).Scan(&id)
	if err != nil {
		t.Fatalf("add blob store %q: %v", name, err)
	}
	return id
}
