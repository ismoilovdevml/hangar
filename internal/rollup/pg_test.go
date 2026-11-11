package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/apitest"
)

func rowCount(t *testing.T, r *Recorder, table string) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRecorderAndStore records two process lifetimes (a restart) and a
// second node, then reads the windows back.
func TestRecorderAndStore(t *testing.T) {
	pool := apitest.MigratedPool(t)
	ctx := context.Background()
	store := NewStore(pool)

	// Nothing recorded yet: zero numbers and no coverage.
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour) // yesterday, midnight
	empty, err := store.Overview(ctx, OverviewQuery{Start: day, End: day.Add(time.Hour), BucketStart: day, Bucket: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !empty.CoverageStart.IsZero() || empty.Requests != 0 || empty.PrevRequests != nil || empty.PeakRequests != 0 ||
		len(empty.Downloads) != 2 || empty.Latency.Total() != 0 {
		t.Fatalf("empty = %+v", empty)
	}

	// Process 1 starts at 01:00 yesterday.
	clock := day.Add(time.Hour)
	src := newSource(t)
	rec := NewRecorder(pool, src.m.Registry, Options{Started: clock})
	rec.now = func() time.Time { return clock }

	// 01:00-01:01: 10 downloads at 20ms, 4 proxy hits (1000 bytes), 1 miss,
	// 3 remote requests at 200ms.
	src.serve(10, "/repository/{repo}/*", "GET", "200", 0.02)
	src.hit("nuget", 4, 1000)
	src.miss("nuget", 1)
	src.remote("nuget-proxy", 3, 0.2)
	clock = clock.Add(30 * time.Second)
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// Same minute again: added to the same row.
	src.serve(2, "/api/v1/repositories", "GET", "200", 0.4)
	clock = clock.Add(15 * time.Second)
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, rec, "traffic_minutes"); n != 1 {
		t.Fatalf("rows after one minute = %d", n)
	}
	// A failed write keeps the increments for the next flush.
	src.serve(5, "/repository/{repo}/*", "GET", "200", 0.02) // 01:00:45 - 01:01:30, attributed to 01:00
	clock = clock.Add(45 * time.Second)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := rec.Flush(cancelled); err == nil {
		t.Fatal("flush with a cancelled context succeeded")
	}
	src.serve(1, "/repository/{repo}/*", "GET", "404", 0.02) // 01:01:30 -, attributed to 01:01
	clock = clock.Add(15 * time.Second)
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// Idle flushes write nothing.
	clock = clock.Add(15 * time.Second)
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rowCount(t, rec, "traffic_minutes"); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}

	// Restart at 02:00: a new process with fresh counters and instance.
	clock = day.Add(2 * time.Hour)
	src2 := newSource(t)
	rec2 := NewRecorder(pool, src2.m.Registry, Options{Started: clock})
	rec2.now = func() time.Time { return clock }
	src2.serve(20, "/repository/{repo}/*", "GET", "200", 0.02)
	src2.hit("npm", 1, 500)
	src2.remote("nuget-proxy", 1, 3)
	clock = clock.Add(15 * time.Second)
	if err := rec2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// A second node at the same minute.
	node2 := newSource(t)
	rec3 := NewRecorder(pool, node2.m.Registry, Options{Started: day.Add(2 * time.Hour)})
	rec3.now = func() time.Time { return clock }
	node2.serve(3, "/v2/*", "GET", "200", 0.02)
	node2.serve(2, "/repository/{repo}/*", "PUT", "401", 0.02)
	node2.serve(1, "/api/v1/repositories", "GET", "403", 0.02)
	node2.serve(1, "/repository/{repo}/*", "GET", "502", 0.02)
	// One of the two 401s answered a request without credentials.
	node2.m.HTTPChallenges.WithLabelValues("PUT", "/repository/{repo}/*").Inc()
	if err := rec3.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := apitest.QueryString(t, pool, `SELECT since::text FROM traffic_coverage`); got == "" {
		t.Fatal("no coverage")
	}

	// Window 01:00-03:00.
	o, err := store.Overview(ctx, OverviewQuery{Start: day.Add(time.Hour), End: day.Add(3 * time.Hour),
		BucketStart: day.Add(time.Hour), Bucket: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !o.CoverageStart.Equal(day.Add(time.Hour)) {
		t.Errorf("coverage = %v", o.CoverageStart)
	}
	if o.Requests != 45 { // 10+2+5+1 + 20 + 3+2+1+1
		t.Errorf("requests = %d", o.Requests)
	}
	if o.PrevRequests != nil {
		t.Errorf("previous window before coverage = %v", *o.PrevRequests)
	}
	if o.PeakRequests < o.Requests {
		t.Errorf("peak %d < total %d", o.PeakRequests, o.Requests)
	}
	if len(o.Downloads) != 2 || o.Downloads[0] != 15 || o.Downloads[1] != 23 {
		t.Errorf("downloads = %v", o.Downloads)
	}
	if len(o.BucketRequests) != 2 || o.BucketRequests[0] != 18 || o.BucketRequests[1] != 27 {
		t.Errorf("bucket requests = %v", o.BucketRequests)
	}
	if o.ServerErrors != 1 || o.Denied != 2 || o.Challenges != 1 {
		t.Errorf("server errors %d denied %d challenges %d, want 1 2 1", o.ServerErrors, o.Denied, o.Challenges)
	}
	if o.CacheHits != 5 || o.CacheMisses != 1 || o.CacheHitBytes != 1500 {
		t.Errorf("cache = %d/%d/%d", o.CacheHits, o.CacheMisses, o.CacheHitBytes)
	}
	if o.Latency.Total() != 45 {
		t.Errorf("latency observations = %d", o.Latency.Total())
	}
	if p95, ok := o.Latency.Quantile(RequestBounds, .95); !ok || p95 <= .01 || p95 > .025 {
		t.Errorf("p95 = %v", p95) // 43 of 45 at 20ms
	}

	// Window 02:00-03:00 only sees the second process and node; its previous
	// hour is fully recorded, and the busiest hour is the current one.
	o2, err := store.Overview(ctx, OverviewQuery{Start: day.Add(2 * time.Hour), End: day.Add(3 * time.Hour),
		BucketStart: day.Add(2 * time.Hour), Bucket: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if o2.Requests != 27 || o2.PrevRequests == nil || *o2.PrevRequests != 18 || o2.CacheHits != 1 {
		t.Errorf("02:00 window = %+v prev %v", o2, o2.PrevRequests)
	}
	if o2.PeakRequests != 27 {
		t.Errorf("peak = %d", o2.PeakRequests)
	}

	// A one-hour window at 03:00-04:00 is quiet. No hour holds both the
	// 01:00 and the 02:00 minutes, so the busiest hour since coverage is one
	// that holds the 02:00 minute (27 requests).
	o3, err := store.Overview(ctx, OverviewQuery{Start: day.Add(3 * time.Hour), End: day.Add(4 * time.Hour),
		BucketStart: day.Add(3 * time.Hour), Bucket: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if o3.Requests != 0 || o3.PeakRequests != 27 || o3.ServerErrors+o3.Denied != 0 || o3.CacheHits+o3.CacheMisses != 0 {
		t.Errorf("quiet window = %+v", o3)
	}

	// Upstream latency per repository: 3 at 200ms and 1 at 3s.
	lat, err := store.UpstreamLatency(ctx, day, day.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	h := lat["nuget-proxy"]
	if h.Total() != 4 || len(lat) != 1 {
		t.Fatalf("upstream latency = %v", lat)
	}
	if p50, _ := h.Quantile(UpstreamBounds, .5); p50 <= .1 || p50 > .25 {
		t.Errorf("p50 = %v", p50)
	}
	if late, _ := store.UpstreamLatency(ctx, day.Add(2*time.Hour), day.Add(3*time.Hour)); late["nuget-proxy"].Total() != 1 {
		t.Errorf("upstream latency after restart = %v", late)
	}

	// Retention: rows older than the retention are pruned on the hourly pass.
	apitest.Exec(t, pool, `INSERT INTO traffic_minutes (minute, instance, requests) VALUES ($1, gen_random_uuid(), 9)`,
		clock.Add(-DefaultRetention-time.Hour))
	apitest.Exec(t, pool, `INSERT INTO upstream_minutes (minute, instance, repository, latency) VALUES ($1, gen_random_uuid(), 'old', '{1}')`,
		clock.Add(-DefaultRetention-time.Hour))
	rec2.pruned = time.Time{}
	if err := rec2.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := apitest.QueryString(t, pool, `SELECT count(*)::text FROM traffic_minutes WHERE requests = 9`); n != "0" {
		t.Errorf("old traffic rows kept: %s", n)
	}
	if n := apitest.QueryString(t, pool, `SELECT count(*)::text FROM upstream_minutes WHERE repository = 'old'`); n != "0" {
		t.Errorf("old upstream rows kept: %s", n)
	}
}

// Run writes a last time when its context ends.
func TestRunFlushesOnStop(t *testing.T) {
	pool := apitest.MigratedPool(t)
	src := newSource(t)
	rec := NewRecorder(pool, src.m.Registry, Options{Started: time.Now(), Interval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rec.Run(ctx); close(done) }()
	src.serve(4, "/repository/{repo}/*", "GET", "200", 0.02)
	cancel()
	<-done
	o, err := NewStore(pool).Overview(context.Background(), OverviewQuery{Start: time.Now().Add(-time.Hour),
		End: time.Now().Add(time.Minute), BucketStart: time.Now().Add(-time.Hour), Bucket: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if o.Requests != 4 || o.CoverageStart.IsZero() {
		t.Errorf("after stop = %+v", o)
	}
	if o.PeakRequests != 4 {
		t.Errorf("peak = %d", o.PeakRequests)
	}
}
