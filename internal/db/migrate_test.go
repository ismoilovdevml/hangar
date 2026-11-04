package db

import (
	"context"
	"testing"
)

// An installation upgraded from a release that already applied a higher
// migration number must still apply the lower-numbered ones a later release
// brings (migration ranges are allotted per feature branch).
func TestMigrationsOutOfOrder(t *testing.T) {
	pool := isolatedPool(t)
	ctx := context.Background()
	m, err := NewMigrator(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	st, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) < 3 {
		t.Skip("needs at least three migrations")
	}
	last := st[len(st)-1].Version
	skipped := st[len(st)-2].Version
	// Apply everything except the second-to-last, as if an older release had
	// shipped only the last one.
	for _, s := range st[:len(st)-2] {
		if _, err := m.provider.ApplyVersion(ctx, s.Version, true); err != nil {
			t.Fatalf("apply %d: %v", s.Version, err)
		}
	}
	if _, err := m.provider.ApplyVersion(ctx, last, true); err != nil {
		t.Fatalf("apply %d: %v", last, err)
	}
	if err := m.Up(ctx, nil); err != nil {
		t.Fatalf("up with an out-of-order migration pending: %v", err)
	}
	after, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range after {
		if !s.Applied {
			t.Errorf("migration %d not applied", s.Version)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM goose_db_version WHERE version_id = $1 AND is_applied", skipped).Scan(&n); err != nil || n != 1 {
		t.Fatalf("migration %d applied %d times (err=%v)", skipped, n, err)
	}
}
