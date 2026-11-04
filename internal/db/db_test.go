package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvTestDatabaseURL names the PostgreSQL used by integration tests. Tests that
// need a database are skipped when it is unset.
const EnvTestDatabaseURL = "HANGAR_TEST_DATABASE_URL"

func TestMigrationFilesAreSequential(t *testing.T) {
	entries, err := fs.ReadDir(Migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations embedded")
	}
	// Numbers are unique and ascending; gaps are allowed so parallel work can
	// reserve ranges. goose applies files in numeric order.
	prev := 0
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		n, err := strconv.Atoi(prefix)
		if !ok || err != nil || len(prefix) != 5 {
			t.Errorf("%s: want an NNNNN_ prefix", e.Name())
			continue
		}
		if n <= prev {
			t.Errorf("%s: number %d is not greater than %d", e.Name(), n, prev)
		}
		prev = n
		b, _ := fs.ReadFile(Migrations(), e.Name())
		if !strings.Contains(string(b), "-- +goose Down") {
			t.Errorf("%s has no Down section", e.Name())
		}
	}
}

// isolatedPool connects to the test database inside a fresh schema that is
// dropped afterwards, so parallel runs and developer data are never touched.
func isolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv(EnvTestDatabaseURL)
	if base == "" {
		t.Skipf("%s not set; skipping database integration test", EnvTestDatabaseURL)
	}
	ctx := context.Background()
	admin, err := Open(ctx, base, PoolOptions{MaxConns: 2})
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
	pool, err := Open(ctx, u.String(), PoolOptions{MaxConns: 4})
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
	return pool
}

func TestMigrationsUpDown(t *testing.T) {
	pool := isolatedPool(t)
	ctx := context.Background()
	m, err := NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	if err := m.Up(ctx, nil); err != nil {
		t.Fatalf("up: %v", err)
	}
	st, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if !s.Applied {
			t.Errorf("migration %s not applied", s.Name)
		}
	}
	if pending, _ := m.HasPending(ctx); pending {
		t.Error("pending after up")
	}

	wantTables := []string{"blob_stores", "blobs", "repositories", "group_members", "components", "assets",
		"users", "roles", "privileges", "role_bindings", "tokens", "identity_providers", "cleanup_policies",
		"policy_repos", "tasks", "task_runs", "audit_events", "nodes", "replication_targets",
		"replication_state", "findings"}
	for _, tbl := range wantTables {
		var ok bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", tbl).Scan(&ok); err != nil || !ok {
			t.Errorf("table %s missing (err=%v)", tbl, err)
		}
	}

	checkSchemaBehaviour(t, pool)

	if err := m.DownTo(ctx, 0, nil); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if v, _ := m.Version(ctx); v != 0 {
		t.Fatalf("version after down = %d", v)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = current_schema() AND tablename <> 'goose_db_version'`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("tables left after down: %d (err=%v)", left, err)
	}
	// Up again: Down sections must leave a clean slate.
	if err := m.Up(ctx, nil); err != nil {
		t.Fatalf("second up: %v", err)
	}
}

func checkSchemaBehaviour(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}
	// Built-in roles are seeded.
	var roles int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM roles WHERE builtin").Scan(&roles); err != nil || roles != 4 {
		t.Errorf("builtin roles = %d (err=%v)", roles, err)
	}
	// Repository name rule is enforced by the database too.
	if err := exec(`INSERT INTO repositories (name, format, type) VALUES ('Bad_Name', 'raw', 'group')`); err == nil {
		t.Error("invalid repository name accepted")
	}
	if err := exec(`INSERT INTO repositories (name, format, type) VALUES ('raw-all', 'raw', 'group')`); err != nil {
		t.Errorf("group insert: %v", err)
	}
	// Hosted repositories need a blob store.
	if err := exec(`INSERT INTO repositories (name, format, type) VALUES ('raw-hosted', 'raw', 'hosted')`); err == nil {
		t.Error("hosted repository without blob store accepted")
	}
	// Audit log: insert works (current-month partition exists), update is rejected.
	if err := exec(`INSERT INTO audit_events (actor, action, target, ip) VALUES ('alice', 'test', 'repo:x', '10.0.0.1')`); err != nil {
		t.Fatalf("audit insert: %v", err)
	}
	var part string
	if err := pool.QueryRow(ctx, `SELECT tableoid::regclass::text FROM audit_events LIMIT 1`).Scan(&part); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(part, "audit_events_2") {
		t.Errorf("audit row landed in %s, want a monthly partition", part)
	}
	if err := exec(`UPDATE audit_events SET actor = 'mallory'`); err == nil {
		t.Error("audit update accepted")
	}
	if err := exec(`DELETE FROM audit_events`); err == nil {
		t.Error("audit delete accepted")
	}
	var created int
	if err := pool.QueryRow(ctx, `SELECT hangar_ensure_audit_partitions(3)`).Scan(&created); err != nil || created != 1 {
		t.Errorf("ensure partitions created %d (err=%v), want 1 new", created, err)
	}
	// Privilege syntax is checked.
	if err := exec(`INSERT INTO privileges (role_id, privilege) SELECT id, 'repo:*:fly' FROM roles LIMIT 1`); err == nil {
		t.Error("invalid privilege accepted")
	}
}
