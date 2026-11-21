package audit

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
)

// partitionOf names the partitions holding the events of actor.
func partitionOf(t *testing.T, pool *pgxpool.Pool, actor string) string {
	t.Helper()
	return apitest.QueryString(t, pool,
		"SELECT coalesce(string_agg(DISTINCT tableoid::regclass::text, ','), '') FROM audit_events WHERE actor = $1", actor)
}

func TestPartitionMaintenance(t *testing.T) {
	pool := apitest.MigratedPool(t)
	ctx := context.Background()
	// Events of months without a partition land in the default partition:
	// one from a clock months ahead, two from long ago.
	future := time.Now().AddDate(0, 5, 0)
	apitest.Exec(t, pool, `INSERT INTO audit_events (at, actor, action)
		VALUES ($1, 'ahead', 'x'), ('2020-03-05 12:00', 'old', 'x'), ('2020-03-06 12:00', 'old', 'x')`, future)
	if p := partitionOf(t, pool, "ahead"); p != "audit_events_default" {
		t.Fatalf("future event in %q", p)
	}
	// Creating the partition of such a month used to fail on these rows;
	// now each such month gets its partition and its rows move into it.
	if _, err := EnsurePartitions(ctx, pool, 6); err != nil {
		t.Fatalf("EnsurePartitions with rows in the default partition: %v", err)
	}
	wantAhead := "audit_events_" + apitest.QueryString(t, pool, "SELECT to_char($1::timestamptz, 'YYYYMM')", future)
	if p := partitionOf(t, pool, "ahead"); p != wantAhead {
		t.Errorf("future event in %q after ensure, want %s", p, wantAhead)
	}
	if p := partitionOf(t, pool, "old"); p != "audit_events_202003" {
		t.Errorf("old events in %q after ensure", p)
	}
	if n := apitest.QueryString(t, pool, "SELECT count(*)::text FROM audit_events_default"); n != "0" {
		t.Errorf("default partition keeps %s rows", n)
	}
	// Moving rows leaves the log append-only: neither the new partitions
	// nor the default partition accept updates or deletes.
	apitest.Exec(t, pool, `INSERT INTO audit_events (at, actor, action) VALUES ($1, 'later', 'x')`, future.AddDate(2, 0, 0))
	for _, sql := range []string{
		`UPDATE audit_events SET actor = 'mallory' WHERE actor = 'old'`,
		`DELETE FROM audit_events WHERE actor = 'ahead'`,
		`DELETE FROM audit_events_default`,
	} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s accepted", sql)
		}
	}

	// Retention drops whole months older than the cutoff and nothing newer.
	dropped, err := DropExpiredPartitions(ctx, pool, 365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dropped, []string{"audit_events_202003"}) {
		t.Errorf("dropped %v, want the 2020-03 partition", dropped)
	}
	if n := apitest.QueryString(t, pool, "SELECT count(*)::text FROM audit_events WHERE actor IN ('ahead', 'later')"); n != "2" {
		t.Errorf("retention removed newer events: %s left", n)
	}
	if dropped, err := DropExpiredPartitions(ctx, pool, 0); err != nil || dropped != nil {
		t.Errorf("retention 0 dropped %v (err=%v)", dropped, err)
	}
	// The event two years ahead gets its partition, then nothing is left.
	if n, err := EnsurePartitions(ctx, pool, 6); err != nil || n != 1 || partitionOf(t, pool, "later") == "audit_events_default" {
		t.Errorf("second ensure created %d (err=%v), want 1 for the later event", n, err)
	}
	if n, err := EnsurePartitions(ctx, pool, 6); err != nil || n != 0 {
		t.Errorf("third ensure created %d (err=%v), want 0", n, err)
	}
}
