package rollup

import (
	"math"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ismoilovdevml/hangar/internal/observability"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// source is a registry with the series Read understands. The proxy series
// are registered locally (the proxy package's are process globals).
type source struct {
	m        *observability.Metrics
	cache    *prometheus.CounterVec
	hitBytes *prometheus.CounterVec
	upstream *prometheus.HistogramVec
}

func newSource(t *testing.T) *source {
	t.Helper()
	s := &source{m: observability.NewMetrics()}
	s.cache = prometheus.NewCounterVec(prometheus.CounterOpts{Name: proxy.MetricCacheRequests, Help: "x"}, []string{"format", "result"})
	s.hitBytes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: proxy.MetricCacheHitBytes, Help: "x"}, []string{"format"})
	s.upstream = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: proxy.MetricUpstreamDuration, Help: "x",
		Buckets: proxy.UpstreamDurationBuckets}, []string{"repository"})
	s.m.Registry.MustRegister(s.cache, s.hitBytes, s.upstream)
	return s
}

func (s *source) serve(n int, route, method, code string, secs float64) {
	for range n {
		s.m.HTTPRequests.WithLabelValues(method, route, code).Inc()
		s.m.HTTPDuration.WithLabelValues(method, route).Observe(secs)
	}
}

func (s *source) hit(format string, n int, size int64) {
	s.cache.WithLabelValues(format, proxy.CacheResultHit).Add(float64(n))
	s.hitBytes.WithLabelValues(format).Add(float64(size))
}

func (s *source) miss(format string, n int) {
	s.cache.WithLabelValues(format, proxy.CacheResultMiss).Add(float64(n))
}

func (s *source) remote(repo string, n int, secs float64) {
	for range n {
		s.upstream.WithLabelValues(repo).Observe(secs)
	}
}

func TestRead(t *testing.T) {
	s := newSource(t)
	if c := Read(s.m.Registry); !c.IsZero() || len(c.Latency) != len(RequestBounds)+1 {
		t.Fatalf("fresh = %+v", c)
	}
	s.serve(10, "/repository/{repo}/*", "GET", "200", 0.02)
	s.serve(2, "/repository/{repo}/*", "GET", "404", 0.02)
	s.serve(3, "/v2/*", "GET", "206", 0.02)
	s.serve(4, "/repository/{repo}/*", "PUT", "201", 0.3)
	s.serve(5, "/api/v1/components", "GET", "200", 0.4)
	s.serve(50, "/healthz", "GET", "200", 0.001)
	s.serve(7, "/api/v1/events", "GET", "200", 120)
	s.serve(1, "/metrics", "GET", "200", 0.001)
	s.serve(3, "/healthz", "GET", "503", 0.001)
	s.hit("nuget", 6, 600)
	s.hit("npm", 2, 50)
	s.miss("nuget", 3)
	s.remote("nuget-proxy", 4, 0.2)
	s.remote("npm-proxy", 1, 3)

	c := Read(s.m.Registry)
	if c.Requests != 24 || c.Downloads != 13 {
		t.Errorf("requests %d downloads %d, want 24 13", c.Requests, c.Downloads)
	}
	if c.ServerErrors != 0 || c.Denied != 0 {
		t.Errorf("outcomes of uncounted routes leaked: %d %d", c.ServerErrors, c.Denied)
	}
	if c.CacheHits != 8 || c.CacheMisses != 3 || c.CacheHitBytes != 650 {
		t.Errorf("cache = %d/%d/%d", c.CacheHits, c.CacheMisses, c.CacheHitBytes)
	}
	// 15 at 20ms, 4 at 300ms, 5 at 400ms: cumulative at .025 is 15, at .5 is 24.
	if c.Latency.Total() != 24 || c.Latency[slices.Index(RequestBounds, .025)] != 15 ||
		c.Latency[slices.Index(RequestBounds, .25)] != 15 || c.Latency[slices.Index(RequestBounds, .5)] != 24 {
		t.Errorf("latency = %v", c.Latency)
	}
	nu := c.Upstreams["nuget-proxy"]
	if nu.Total() != 4 || nu[slices.Index(UpstreamBounds, .1)] != 0 || nu[slices.Index(UpstreamBounds, .25)] != 4 {
		t.Errorf("nuget-proxy = %v", nu)
	}
	if c.Upstreams["npm-proxy"].Total() != 1 {
		t.Errorf("npm-proxy = %v", c.Upstreams["npm-proxy"])
	}

	// Server errors (5xx) and refusals (401, 403) of counted routes.
	s.serve(2, "/repository/{repo}/*", "PUT", "401", 0.3)
	s.serve(1, "/api/v1/components", "GET", "403", 0.4)
	s.serve(1, "/api/v1/components", "GET", "500", 0.4)
	s.serve(1, "/repository/{repo}/*", "GET", "502", 0.02)
	c = Read(s.m.Registry)
	if c.Requests != 29 || c.ServerErrors != 2 || c.Denied != 3 {
		t.Errorf("requests %d server errors %d denied %d, want 29 2 3", c.Requests, c.ServerErrors, c.Denied)
	}
	if c.Latency.Total() != 29 {
		t.Errorf("latency observations = %d", c.Latency.Total())
	}
}

func TestSubAdd(t *testing.T) {
	s := newSource(t)
	s.serve(5, "/repository/{repo}/*", "GET", "200", 0.02)
	s.remote("a", 2, 0.03)
	before := Read(s.m.Registry)
	s.serve(3, "/repository/{repo}/*", "GET", "200", 0.02)
	s.hit("raw", 1, 10)
	s.remote("b", 1, 0.03)
	d := Read(s.m.Registry).Sub(before)
	if d.Requests != 3 || d.Downloads != 3 || d.CacheHits != 1 || d.CacheHitBytes != 10 || d.Latency.Total() != 3 {
		t.Errorf("delta = %+v", d)
	}
	if _, ok := d.Upstreams["a"]; ok || d.Upstreams["b"].Total() != 1 {
		t.Errorf("upstream delta = %v", d.Upstreams)
	}
	// A counter that went backwards restarted: never negative.
	if back := before.Sub(Read(s.m.Registry)); !back.IsZero() {
		t.Errorf("backwards = %+v", back)
	}
	sum := d.Add(d)
	if sum.Requests != 6 || sum.Latency.Total() != 6 || sum.Upstreams["b"].Total() != 2 || d.Upstreams["b"].Total() != 1 {
		t.Errorf("sum = %+v (d = %+v)", sum, d)
	}
	if (Counts{}).Add(Counts{}).IsZero() != true {
		t.Error("zero + zero")
	}
}

func TestQuantile(t *testing.T) {
	bounds := []float64{.1, .5, 1}
	if _, ok := (Histogram{0, 0, 0, 0}).Quantile(bounds, .5); ok {
		t.Error("empty histogram has a quantile")
	}
	if _, ok := (Histogram{1, 2}).Quantile(bounds, .5); ok {
		t.Error("histogram of the wrong length has a quantile")
	}
	// 10 observations: 2 <= .1, 8 in (.1, .5].
	h := Histogram{2, 10, 10, 10}
	if v, _ := h.Quantile(bounds, .5); math.Abs(v-0.25) > 1e-9 { // rank 5: 3/8 into (.1, .5]
		t.Errorf("p50 = %v", v)
	}
	if v, _ := h.Quantile(bounds, .1); math.Abs(v-0.05) > 1e-9 {
		t.Errorf("p10 = %v", v)
	}
	// Everything above the largest bound reports that bound.
	if v, _ := (Histogram{0, 0, 0, 5}).Quantile(bounds, .5); v != 1 {
		t.Errorf("overflow p50 = %v", v)
	}
}

// The stored bounds equal the source buckets, so the mapping is exact.
func TestBoundsMatchSources(t *testing.T) {
	if !slices.Equal(UpstreamBounds, proxy.UpstreamDurationBuckets) {
		t.Errorf("UpstreamBounds %v != proxy buckets %v", UpstreamBounds, proxy.UpstreamDurationBuckets)
	}
	m := observability.NewMetrics()
	m.HTTPDuration.WithLabelValues("GET", "/x").Observe(1)
	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	for _, f := range fams {
		if f.GetName() == MetricHTTPDuration {
			for _, b := range f.GetMetric()[0].GetHistogram().GetBucket() {
				got = append(got, b.GetUpperBound())
			}
		}
	}
	if !slices.Equal(got, RequestBounds) {
		t.Errorf("RequestBounds %v != http buckets %v", RequestBounds, got)
	}
}

// Source buckets that differ from the stored bounds map to the largest
// source bucket not above each bound.
func TestReadMapsForeignBuckets(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: proxy.MetricUpstreamDuration, Help: "x",
		Buckets: []float64{0.03, 0.2, 2}}, []string{"repository"})
	reg.MustRegister(h)
	h.WithLabelValues("r").Observe(0.01) // <= .03
	h.WithLabelValues("r").Observe(0.1)  // <= .2
	h.WithLabelValues("r").Observe(1)    // <= 2
	got := Read(reg).Upstreams["r"]
	want := Histogram{0, 1, 1, 2, 2, 2, 3, 3, 3, 3, 3, 3} // .025 .05 .1 .25 .5 1 2.5 5 10 30 60, total
	if !slices.Equal(got, want) {
		t.Errorf("mapped = %v, want %v", got, want)
	}
}
