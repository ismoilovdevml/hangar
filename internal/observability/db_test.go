package observability

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
	"github.com/ismoilovdevml/hangar/internal/db"
	"github.com/ismoilovdevml/hangar/internal/health"
)

func TestQueryTracerAndPoolMetrics(t *testing.T) {
	url := os.Getenv(apitest.EnvTestDatabaseURL)
	if url == "" {
		t.Skipf("%s not set", apitest.EnvTestDatabaseURL)
	}
	m := NewMetrics()
	rec := recordSpans(t)
	ctx := t.Context()
	pool, err := db.Open(ctx, url, db.PoolOptions{MaxConns: 2, Tracer: NewQueryTracer(m)})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := m.RegisterDBPool(pool); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT $1::int", 7).Scan(&n); err != nil || n != 7 {
		t.Fatalf("select = %d %v", n, err)
	}
	if _, err := pool.Exec(ctx, "SELECT * FROM no_such_table_for_metrics"); err == nil {
		t.Fatal("query on a missing table succeeded")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "CREATE TEMP TABLE obs_copy (v int)"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.CopyFrom(ctx, pgx.Identifier{"obs_copy"}, []string{"v"}, pgx.CopyFromRows([][]any{{1}, {2}})); err != nil {
		t.Fatal(err)
	}
	b := &pgx.Batch{}
	b.Queue("SELECT 1")
	b.Queue("SELECT 2")
	if err := conn.SendBatch(ctx, b).Close(); err != nil {
		t.Fatal(err)
	}

	g := m.Registry
	for _, op := range []string{"select", "copy", "batch", "other"} {
		if counterValue(t, g, "hangar_db_query_duration_seconds", "op="+op) == 0 {
			t.Errorf("no %s latency recorded", op)
		}
	}
	if got := counterValue(t, g, "hangar_db_query_errors_total", "op=select"); got != 1 {
		t.Errorf("select errors = %v", got)
	}
	if counterValue(t, g, "hangar_db_pool_acquires_total") == 0 || counterValue(t, g, "hangar_db_pool_max_connections") != 2 {
		t.Error("pool statistics missing")
	}
	var sawStatement bool
	for _, s := range rec.Ended() {
		for _, a := range s.Attributes() {
			if a.Key == "db.query.text" && strings.Contains(a.Value.AsString(), "SELECT $1::int") {
				sawStatement = true
			}
			if strings.Contains(a.Value.String(), "7") && a.Key != "db.query.text" && a.Key != "db.operation.batch.size" {
				t.Errorf("argument value recorded in span attribute %s", a.Key)
			}
		}
	}
	if !sawStatement {
		t.Errorf("no db span with the statement; spans = %v", spanNames(rec))
	}
}

func TestStorageUsageAndReadiness(t *testing.T) {
	pool := apitest.MigratedPool(t)
	dir := t.TempDir()
	apitest.Exec(t, pool, `INSERT INTO blob_stores (name, kind, config, soft_quota_bytes) VALUES
		('default', 'fs', jsonb_build_object('path', $1::text), 1000), ('bucket', 's3', '{}', NULL)`, dir)
	apitest.Exec(t, pool, `INSERT INTO blobs (store_id, digest, size, soft_deleted_at)
		SELECT id, 'sha256:' || repeat(d, 64), sz, del FROM blob_stores,
		(VALUES ('a', 300, NULL::timestamptz), ('b', 200, NULL), ('c', 50, now())) v(d, sz, del) WHERE name = 'default'`)

	m := NewMetrics()
	su, err := m.RegisterStorageUsage(pool, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if family(t, m.Registry, "hangar_blobstore_used_bytes") != nil {
		t.Error("usage reported before the first refresh")
	}
	if err := su.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	g := m.Registry
	for _, c := range []struct {
		name string
		lbl  []string
		want float64
	}{
		{"hangar_blobstore_used_bytes", []string{"store=default", "driver=fs"}, 500},
		{"hangar_blobstore_reclaimable_bytes", []string{"store=default"}, 50},
		{"hangar_blobstore_soft_quota_bytes", []string{"store=default"}, 1000},
		{"hangar_blobstore_used_bytes", []string{"store=bucket", "driver=s3"}, 0},
	} {
		if got := counterValue(t, g, c.name, c.lbl...); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.lbl, got, c.want)
		}
	}
	if f := family(t, g, "hangar_blobstore_soft_quota_bytes"); f == nil || len(f.GetMetric()) != 1 {
		t.Error("quota reported for a store without one")
	}
	if counterValue(t, g, "hangar_blobstore_fs_size_bytes", "store=default") == 0 || family(t, g, "hangar_blobstore_fs_free_bytes") == nil {
		t.Error("filesystem size of the fs store missing")
	}

	hc := health.New(time.Second)
	hc.Add("database", pool.Ping)
	hc.Add("eventbus", func(context.Context) error { return context.DeadlineExceeded })
	if err := m.RegisterReadiness(hc); err != nil {
		t.Fatal(err)
	}
	if counterValue(t, g, "hangar_ready") != 0 || counterValue(t, g, "hangar_dependency_up", "dependency=database") != 1 ||
		counterValue(t, g, "hangar_dependency_up", "dependency=eventbus") != 0 {
		t.Error("readiness gauges wrong")
	}
}
