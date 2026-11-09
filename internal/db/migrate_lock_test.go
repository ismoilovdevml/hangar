package db

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// Several nodes starting at once with migrate_on_start (or a migrate Job
// next to a starting node) all call Up: exactly one applies the migrations,
// the others wait for it and then find nothing pending. "upgrade" is the
// common case: an existing schema with the newest migrations pending.
func TestMigrateUpConcurrentNodes(t *testing.T) {
	files, err := fileVersions()
	if err != nil {
		t.Fatal(err)
	}
	const upgradePending = 6
	for _, tc := range []struct {
		name    string
		pending int
	}{{"fresh", len(files)}, {"upgrade", upgradePending}} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := isolatedURL(t)
			ctx := context.Background()
			if tc.pending < len(files) {
				m, err := NewMigrator(poolFor(t, dsn, 2))
				if err != nil {
					t.Fatal(err)
				}
				st, err := m.Status(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := m.provider.UpTo(ctx, st[len(st)-tc.pending-1].Version); err != nil {
					t.Fatal(err)
				}
				_ = m.Close()
			}
			logs, errs := upConcurrently(t, dsn, 4)
			appliers, applied := 0, 0
			for i := range logs {
				if errs[i] != nil {
					t.Errorf("node %d: %v", i, errs[i])
				}
				n := strings.Count(logs[i], "migration applied")
				if n > 0 {
					appliers++
				}
				applied += n
				if strings.Contains(logs[i], "duplicate key") || strings.Contains(logs[i], "already exists") {
					t.Errorf("node %d logged a conflicting migration:\n%s", i, logs[i])
				}
			}
			if appliers != 1 || applied != tc.pending {
				t.Fatalf("%d nodes applied %d migrations; want 1 node applying all %d", appliers, applied, tc.pending)
			}
			var rows, distinct int
			if err := poolFor(t, dsn, 1).QueryRow(ctx,
				`SELECT count(*), count(DISTINCT version_id) FROM goose_db_version WHERE version_id > 0`).Scan(&rows, &distinct); err != nil {
				t.Fatal(err)
			}
			if rows != len(files) || distinct != len(files) {
				t.Fatalf("goose_db_version has %d rows for %d versions; want %d each", rows, distinct, len(files))
			}
		})
	}
}

// upConcurrently runs Up on n migrators, each on its own pool of dsn, all
// released at the same moment, and returns their logs and errors.
func upConcurrently(t *testing.T, dsn string, n int) ([]string, []error) {
	t.Helper()
	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
		errs  = make([]error, n)
		bufs  = make([]*bytes.Buffer, n)
	)
	for i := range n {
		// Two connections, like `hangar migrate up`: the lock pins one.
		m, err := NewMigrator(poolFor(t, dsn, 2))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close() })
		bufs[i] = &bytes.Buffer{}
		log := slog.New(slog.NewTextHandler(bufs[i], nil))
		wg.Go(func() {
			<-start
			errs[i] = m.Up(context.Background(), log)
		})
	}
	close(start)
	wg.Wait()
	logs := make([]string, n)
	for i, b := range bufs {
		logs[i] = b.String()
	}
	return logs, errs
}

// While another session holds the migration lock, Up waits (applying
// nothing, not even creating the version table) and proceeds once it is
// released.
func TestMigrateUpWaitsForLock(t *testing.T) {
	dsn := isolatedURL(t)
	ctx := context.Background()
	holder := poolFor(t, dsn, 1)
	conn, err := holder.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+migrationLockKeySQL+`)`); err != nil {
		t.Fatal(err)
	}

	pool := poolFor(t, dsn, 2)
	m, err := NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	var logBuf syncBuffer
	done := make(chan error, 1)
	go func() { done <- m.Up(ctx, slog.New(slog.NewTextHandler(&logBuf, nil))) }()

	select {
	case err := <-done:
		t.Fatalf("Up returned while the lock was held: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if !strings.Contains(logBuf.String(), "waiting for the migration lock") {
		t.Errorf("no waiting message logged: %q", logBuf.String())
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("goose_db_version created while another session held the lock")
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(`+migrationLockKeySQL+`)`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Up after the lock was released: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Up did not finish after the lock was released")
	}
	if pending, err := m.HasPending(ctx, nil); err != nil || pending {
		t.Fatalf("pending after Up = %v, %v", pending, err)
	}
}

// A cancelled wait returns the context error and leaves no lock behind.
func TestMigrateUpCancelledWait(t *testing.T) {
	dsn := isolatedURL(t)
	holder := poolFor(t, dsn, 1)
	conn, err := holder.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock(`+migrationLockKeySQL+`)`); err != nil {
		t.Fatal(err)
	}
	pool := poolFor(t, dsn, 2)
	m, err := NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := m.Up(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Up with a cancelled wait = %v, want context.DeadlineExceeded", err)
	}
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock(`+migrationLockKeySQL+`)`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	// The abandoned wait must not have left the lock taken.
	if err := m.Up(context.Background(), nil); err != nil {
		t.Fatalf("Up after the holder released: %v", err)
	}
}

// The lock is per schema: installations sharing one PostgreSQL database in
// different schemas do not wait for each other.
func TestMigrationLockIsPerSchema(t *testing.T) {
	other := poolFor(t, isolatedURL(t), 1)
	conn, err := other.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock(`+migrationLockKeySQL+`)`); err != nil {
		t.Fatal(err)
	}
	m, err := NewMigrator(isolatedPool(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.Up(ctx, nil); err != nil {
		t.Fatalf("Up blocked by another schema's lock: %v", err)
	}
}

// fileVersions lists the embedded migration files.
func fileVersions() ([]string, error) {
	return fs.Glob(Migrations(), "*.sql")
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// One connection cannot hold the lock and run the migrations.
func TestNewMigratorNeedsTwoConnections(t *testing.T) {
	if _, err := NewMigrator(poolFor(t, isolatedURL(t), 1)); err == nil || !strings.Contains(err.Error(), "at least 2 connections") {
		t.Fatalf("NewMigrator on a 1-connection pool = %v", err)
	}
}
