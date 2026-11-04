package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCacheMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := RegisterMetrics(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterMetrics(reg); err != nil {
		t.Fatalf("second register = %v", err)
	}
	hits0 := testutil.ToFloat64(cacheRequests.WithLabelValues("metrics-test", CacheResultHit))
	CacheHit("metrics-test", 100)
	CacheHit("metrics-test", 0)
	CacheMiss("metrics-test")
	if got := testutil.ToFloat64(cacheRequests.WithLabelValues("metrics-test", CacheResultHit)) - hits0; got != 2 {
		t.Errorf("hits = %v, want 2", got)
	}
	if got := testutil.ToFloat64(cacheRequests.WithLabelValues("metrics-test", CacheResultMiss)); got != 1 {
		t.Errorf("misses = %v, want 1", got)
	}
	if got := testutil.ToFloat64(cacheHitBytes.WithLabelValues("metrics-test")); got != 100 {
		t.Errorf("hit bytes = %v, want 100", got)
	}
	fams, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range fams {
		names[f.GetName()] = true
	}
	if !names[MetricCacheRequests] || !names[MetricCacheHitBytes] {
		t.Errorf("gathered %v", names)
	}
}

func TestCacheTally(t *testing.T) {
	val := func(result string) float64 {
		return testutil.ToFloat64(cacheRequests.WithLabelValues("tally-test", result))
	}
	bytes := func() float64 { return testutil.ToFloat64(cacheHitBytes.WithLabelValues("tally-test")) }

	// Two members hit, one missed: one miss for the request.
	ctx, tally := WithCacheTally(context.Background())
	NoteCacheHit(ctx, 10)
	NoteCacheMiss(ctx)
	NoteCacheHit(ctx, 5)
	tally.Record("tally-test", http.MethodGet)
	if val(CacheResultMiss) != 1 || val(CacheResultHit) != 0 {
		t.Fatalf("miss request: hit=%v miss=%v", val(CacheResultHit), val(CacheResultMiss))
	}
	// Hits only: one hit with the bytes served (SetSize overrides).
	ctx, tally = WithCacheTally(context.Background())
	NoteCacheHit(ctx, 10)
	tally.SetSize(3)
	tally.Record("tally-test", http.MethodGet)
	if val(CacheResultHit) != 1 || bytes() != 3 {
		t.Fatalf("hit request: hit=%v bytes=%v", val(CacheResultHit), bytes())
	}
	// HEAD serves no bytes; a request without notes is not counted.
	ctx, tally = WithCacheTally(context.Background())
	NoteCacheHit(ctx, 10)
	tally.Record("tally-test", http.MethodHead)
	_, empty := WithCacheTally(context.Background())
	empty.Record("tally-test", http.MethodGet)
	NoteCacheHit(context.Background(), 1) // no tally: ignored
	if val(CacheResultHit) != 2 || val(CacheResultMiss) != 1 || bytes() != 3 {
		t.Fatalf("head/empty: hit=%v miss=%v bytes=%v", val(CacheResultHit), val(CacheResultMiss), bytes())
	}
}
