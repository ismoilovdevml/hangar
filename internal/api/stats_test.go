package api

import (
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ismoilovdevml/hangar/internal/observability"
)

func TestRequestStats(t *testing.T) {
	m := observability.NewMetrics()
	cache := prometheus.NewCounterVec(prometheus.CounterOpts{Name: MetricProxyCacheResult, Help: "x"}, []string{"result"})
	m.Registry.MustRegister(cache)
	started := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	clock := started
	s := NewRequestStats(m.Registry, started)
	s.now = func() time.Time { return clock }

	serve := func(n int, route, method, code string, secs float64) {
		for range n {
			m.HTTPRequests.WithLabelValues(method, route, code).Inc()
			m.HTTPDuration.WithLabelValues(method, route).Observe(secs)
		}
	}
	// Hour 1: 100 downloads at 20ms, probes ignored.
	clock = started.Add(time.Hour)
	serve(100, "/repository/{repo}/*", "GET", "200", 0.02)
	serve(50, "/healthz", "GET", "200", 0.001)
	cache.WithLabelValues("hit").Add(30)
	cache.WithLabelValues("miss").Add(10)
	s.Sample()
	// Hour 2: 300 requests, 200 of them API calls at 400ms.
	clock = started.Add(2 * time.Hour)
	serve(100, "/repository/{repo}/*", "GET", "200", 0.02)
	serve(200, "/api/v1/components", "GET", "200", 0.4)
	serve(5, "/repository/{repo}/*", "PUT", "201", 0.02)

	w := s.snapshot()
	rq := w.requests(started.Add(time.Hour))
	if rq.Total == nil || *rq.Total != 305 {
		t.Fatalf("total = %v", rq.Total)
	}
	if rq.TrendPct == nil || *rq.TrendPct != 205 { // 305 vs 100
		t.Errorf("trend = %v", rq.TrendPct)
	}
	if rq.PeakTotal == nil || *rq.PeakTotal != 305 {
		t.Errorf("peak = %v", rq.PeakTotal)
	}
	// 200 of 305 at 0.4s: p95 lies in the (0.25, 0.5] bucket.
	if rq.P95MS == nil || *rq.P95MS <= 250 || *rq.P95MS > 500 {
		t.Errorf("p95 = %v", rq.P95MS)
	}
	// Window reaching back before start: counted from start, no trend.
	all := w.requests(started.Add(-time.Hour))
	if *all.Total != 405 || all.TrendPct != nil || !all.CoverageStart.Equal(started) {
		t.Errorf("all = %+v", all)
	}
	// Interpolation: half of hour 2's downloads fall in its first half.
	if d := w.downloads(started.Add(time.Hour), started.Add(90*time.Minute)); d == nil || *d != 50 {
		t.Errorf("downloads = %v", d)
	}
	if d := w.downloads(started.Add(-2*time.Hour), started.Add(-time.Hour)); d != nil {
		t.Errorf("downloads before start = %v", *d)
	}
	c := w.cache(started)
	if c.HitRatio == nil || math.Abs(*c.HitRatio-0.75) > 1e-9 || c.BytesSaved == nil {
		t.Errorf("cache = %+v", c)
	}
	// No cache metrics registered: null, not zero.
	s2 := NewRequestStats(observability.NewMetrics().Registry, started)
	if cc := s2.snapshot().cache(started); cc.HitRatio != nil || cc.BytesSaved != nil {
		t.Errorf("unknown cache = %+v", cc)
	}
	if q := s2.snapshot().quantile(started, time.Now(), 0.95); q != nil {
		t.Errorf("p95 without traffic = %v", *q)
	}
}
