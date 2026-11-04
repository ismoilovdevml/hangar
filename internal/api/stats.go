package api

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Prometheus series RequestStats reads. The HTTP ones are registered by
// observability.NewMetrics; the proxy cache ones by the proxy subsystem
// (counter vec with label result="hit"|"miss", and bytes served from cache).
const (
	MetricHTTPRequests     = "hangar_http_requests_total"
	MetricHTTPDuration     = "hangar_http_request_duration_seconds"
	MetricProxyCacheResult = "hangar_proxy_cache_requests_total"
	MetricProxyCacheBytes  = "hangar_proxy_cache_hit_bytes_total"
)

// sample vector layout.
const (
	vRequests = iota
	vDownloads
	vCacheHits
	vCacheMisses
	vCacheBytes
	vHistCount
	vHistBuckets // cumulative bucket counts follow
)

type sample struct {
	at         time.Time
	v          []float64
	cacheKnown bool
}

// RequestStats turns this node's Prometheus counters into windowed numbers
// for the Overview screen. Counters only say "since process start", so it
// samples them every Interval and keeps Retention of history in memory
// (about 1.5 MB for 31 days at 5 minutes). Numbers cover this node only and
// restart from zero with the process; windows that reach back before the
// process started report what was observed since then (see coverage).
type RequestStats struct {
	g         prometheus.Gatherer
	started   time.Time
	interval  time.Duration
	retention time.Duration
	now       func() time.Time

	mu      sync.Mutex
	samples []sample // oldest first; samples[0] is the zero sample at start
	bounds  []float64
}

// NewRequestStats reads g. started is the process start (counters were zero then).
func NewRequestStats(g prometheus.Gatherer, started time.Time) *RequestStats {
	return &RequestStats{g: g, started: started, interval: 5 * time.Minute, retention: 31 * 24 * time.Hour,
		now: time.Now, samples: []sample{{at: started, v: make([]float64, vHistBuckets)}}}
}

// Run samples until ctx ends. Start it once: go stats.Run(ctx).
func (s *RequestStats) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sample()
		}
	}
}

// Sample records the current counter values.
func (s *RequestStats) Sample() {
	cur := s.collect()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, cur)
	cut := cur.at.Add(-s.retention)
	i := 1
	for i < len(s.samples)-1 && s.samples[i].at.Before(cut) {
		i++
	}
	if i > 1 {
		s.samples = append(s.samples[:1], s.samples[i:]...)
	}
}

func isFormatRoute(route string) bool {
	return strings.HasPrefix(route, "/repository/") || strings.HasPrefix(route, "/v2/")
}

// countedRoute excludes operational endpoints and the long-lived event stream.
func countedRoute(route string) bool {
	switch route {
	case "/healthz", "/readyz", "/metrics", "unmatched", "/api/v1/events":
		return false
	}
	return !strings.HasSuffix(route, "/metrics")
}

func label(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func (s *RequestStats) collect() sample {
	cur := sample{at: s.now()}
	fams, _ := s.g.Gather() // partial results are still useful
	s.mu.Lock()
	bounds := s.bounds
	s.mu.Unlock()
	var hist map[float64]float64
	var histCount float64
	v := make([]float64, vHistBuckets)
	for _, f := range fams {
		switch f.GetName() {
		case MetricHTTPRequests:
			for _, m := range f.GetMetric() {
				route := label(m, "route")
				if !countedRoute(route) {
					continue
				}
				n := m.GetCounter().GetValue()
				v[vRequests] += n
				code := label(m, "code")
				if isFormatRoute(route) && label(m, "method") == "GET" && (code == "200" || code == "206") {
					v[vDownloads] += n
				}
			}
		case MetricHTTPDuration:
			hist = map[float64]float64{}
			for _, m := range f.GetMetric() {
				if !countedRoute(label(m, "route")) {
					continue
				}
				h := m.GetHistogram()
				histCount += float64(h.GetSampleCount())
				for _, b := range h.GetBucket() {
					hist[b.GetUpperBound()] += float64(b.GetCumulativeCount())
				}
			}
		case MetricProxyCacheResult:
			cur.cacheKnown = true
			for _, m := range f.GetMetric() {
				switch label(m, "result") {
				case "hit":
					v[vCacheHits] += m.GetCounter().GetValue()
				case "miss":
					v[vCacheMisses] += m.GetCounter().GetValue()
				}
			}
		case MetricProxyCacheBytes:
			for _, m := range f.GetMetric() {
				v[vCacheBytes] += m.GetCounter().GetValue()
			}
		}
	}
	if len(hist) > 0 && bounds == nil {
		for b := range hist {
			if !math.IsInf(b, 1) {
				bounds = append(bounds, b)
			}
		}
		sort.Float64s(bounds)
		s.mu.Lock()
		if s.bounds == nil {
			s.bounds = bounds
		}
		bounds = s.bounds
		s.mu.Unlock()
	}
	v[vHistCount] = histCount
	for _, b := range bounds {
		v = append(v, hist[b])
	}
	cur.v = v
	return cur
}

// window is a consistent view: history plus a fresh sample of now.
type window struct {
	samples []sample
	bounds  []float64
	started time.Time
}

func (s *RequestStats) snapshot() window {
	cur := s.collect()
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := make([]sample, len(s.samples), len(s.samples)+1)
	copy(ss, s.samples)
	return window{samples: append(ss, cur), bounds: s.bounds, started: s.started}
}

func (w window) now() sample { return w.samples[len(w.samples)-1] }

func at(v []float64, i int) float64 {
	if i < len(v) {
		return v[i]
	}
	return 0
}

// valueAt interpolates counter i at time t (0 before the process started).
func (w window) valueAt(t time.Time, i int) float64 {
	ss := w.samples
	if !t.After(ss[0].at) {
		return 0
	}
	last := ss[len(ss)-1]
	if !t.Before(last.at) {
		return at(last.v, i)
	}
	j := sort.Search(len(ss), func(k int) bool { return ss[k].at.After(t) }) // ss[j-1].at <= t < ss[j].at
	a, b := ss[j-1], ss[j]
	span := b.at.Sub(a.at).Seconds()
	if span <= 0 {
		return at(b.v, i)
	}
	f := t.Sub(a.at).Seconds() / span
	return at(a.v, i) + f*(at(b.v, i)-at(a.v, i))
}

func (w window) delta(from, to time.Time, i int) float64 {
	return math.Max(w.valueAt(to, i)-w.valueAt(from, i), 0)
}

func ptr[T any](v T) *T { return &v }

// requestNumbers are the requests and cache blocks of OverviewMetrics.
type requestNumbers struct {
	Total         *int64     `json:"total"`
	TrendPct      *float64   `json:"trend_pct"`
	PeakTotal     *int64     `json:"peak_total"`
	P95MS         *float64   `json:"p95_ms"`
	CoverageStart *time.Time `json:"coverage_start"`
}

type cacheNumbers struct {
	HitRatio   *float64 `json:"hit_ratio"`
	BytesSaved *int64   `json:"bytes_saved"`
}

func (w window) requests(start time.Time) requestNumbers {
	end := w.now().at
	length := end.Sub(start)
	out := requestNumbers{CoverageStart: ptr(maxTime(start, w.started).UTC())}
	out.Total = ptr(int64(math.Round(w.delta(start, end, vRequests))))
	if prevStart := start.Add(-length); !prevStart.Before(w.started) {
		if prev := w.delta(prevStart, start, vRequests); prev > 0 {
			out.TrendPct = ptr(math.Round((float64(*out.Total)-prev)/prev*1000) / 10)
		}
	}
	var peak float64
	found := false
	limit := end.Add(-30 * 24 * time.Hour)
	for _, s := range w.samples {
		ws := s.at.Add(-length)
		if ws.Before(w.started) || s.at.Before(limit) {
			continue
		}
		found = true
		peak = math.Max(peak, w.delta(ws, s.at, vRequests))
	}
	if found {
		out.PeakTotal = ptr(int64(math.Round(peak)))
	}
	out.P95MS = w.quantile(start, end, 0.95)
	return out
}

// quantile estimates the q-quantile latency in ms over [start, end] from the
// histogram bucket deltas, interpolating linearly inside the bucket.
func (w window) quantile(start, end time.Time, q float64) *float64 {
	total := w.delta(start, end, vHistCount)
	if total <= 0 || len(w.bounds) == 0 {
		return nil
	}
	rank := q * total
	prevBound, prevCount := 0.0, 0.0
	for k, b := range w.bounds {
		c := w.delta(start, end, vHistBuckets+k)
		if c >= rank {
			v := prevBound
			if c > prevCount {
				v += (b - prevBound) * (rank - prevCount) / (c - prevCount)
			}
			return ptr(math.Round(v*1000*10) / 10)
		}
		prevBound, prevCount = b, c
	}
	// Above the largest finite bucket: report its bound.
	return ptr(w.bounds[len(w.bounds)-1] * 1000)
}

func (w window) cache(start time.Time) cacheNumbers {
	now := w.now()
	if !now.cacheKnown {
		return cacheNumbers{}
	}
	var out cacheNumbers
	hits, misses := w.delta(start, now.at, vCacheHits), w.delta(start, now.at, vCacheMisses)
	if hits+misses > 0 {
		out.HitRatio = ptr(math.Round(hits/(hits+misses)*10000) / 10000)
	}
	out.BytesSaved = ptr(int64(w.delta(start, now.at, vCacheBytes)))
	return out
}

// downloads returns content downloads in [from, to), nil if the window
// ended before the process started.
func (w window) downloads(from, to time.Time) *int64 {
	if !to.After(w.started) {
		return nil
	}
	return ptr(int64(math.Round(w.delta(from, minTime(to, w.now().at), vDownloads))))
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
