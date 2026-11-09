package proxy

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// Cache metric names. internal/rollup reads them for the Overview cache numbers.
const (
	MetricCacheRequests = "hangar_proxy_cache_requests_total"
	MetricCacheHitBytes = "hangar_proxy_cache_hit_bytes_total"
	// MetricUpstreamDuration is the remote latency histogram, labelled by
	// repository; internal/rollup persists it for the Overview screen.
	MetricUpstreamDuration = "hangar_proxy_upstream_request_duration_seconds"
	// MetricNegativeCacheEvictions counts remembered 404s evicted for space.
	MetricNegativeCacheEvictions = "hangar_proxy_negative_cache_evictions_total"
)

// Cache results recorded by CacheHit and CacheMiss.
const (
	CacheResultHit  = "hit"
	CacheResultMiss = "miss"
)

var (
	cacheRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricCacheRequests,
		Help: "Proxy repository content requests by cache result (hit = served from the local cache, miss = fetched from the remote).",
	}, []string{"format", "result"})
	cacheHitBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricCacheHitBytes,
		Help: "Bytes served from the proxy cache without contacting the remote.",
	}, []string{"format"})
)

// Results of hangar_proxy_upstream_requests_total besides status classes
// ("2xx" ... "5xx").
const (
	resultRateLimited = "rate_limited" // 429 after the Retry-After budget, or the host is still gated
	resultError       = "error"        // connection, TLS, timeout or blocked by the address policy
	resultCircuitOpen = "circuit_open" // failed fast: the host's breaker is open
	resultOffline     = "offline"      // the repository is in offline mode
)

// Reasons of hangar_proxy_upstream_retries_total.
const (
	retryReasonError     = "error"
	retryReason5xx       = "5xx"
	retryReasonRateLimit = "rate_limited"
)

// UpstreamDurationBuckets are the bucket bounds (seconds) of
// MetricUpstreamDuration.
var UpstreamDurationBuckets = []float64{0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

var (
	requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hangar_proxy_upstream_requests_total",
		Help: "Requests from proxy repositories to their remotes and related hosts, by final result (status class, rate_limited, error, circuit_open, offline). Retries are counted once.",
	}, []string{"repository", "host", "result"})
	requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    MetricUpstreamDuration,
		Help:    "Time from sending a proxy remote request to its response headers, including retries.",
		Buckets: UpstreamDurationBuckets,
	}, []string{"repository"})
	retriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hangar_proxy_upstream_retries_total",
		Help: "Retried proxy remote requests by reason (error, 5xx, rate_limited).",
	}, []string{"repository", "reason"})
	resumesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hangar_proxy_upstream_resumes_total",
		Help: "Interrupted proxy remote downloads continued with a Range request.",
	}, []string{"repository"})
	inflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hangar_proxy_upstream_inflight_requests",
		Help: "Proxy remote requests in flight (waiting for a slot, waiting for headers or streaming a body).",
	}, []string{"repository"})
	negativeEvictions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricNegativeCacheEvictions,
		Help: "Remembered remote 404s dropped before their TTL because the repository's negative cache was full.",
	}, []string{"repository"})
)

// RegisterMetrics registers the proxy cache and upstream metrics on r.
// Registering on a registry that already has them is not an error.
func RegisterMetrics(r prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{cacheRequests, cacheHitBytes, requestsTotal, requestDuration, retriesTotal, resumesTotal, inflight, negativeEvictions} {
		if err := r.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if !errors.As(err, &are) {
				return err
			}
		}
	}
	return nil
}

// statusClass is the requests_total result of an HTTP status.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

// CacheHit records a proxy request of format answered from the local cache
// with size bytes (0 if unknown, e.g. a HEAD or a 304 to the client).
// Formats call it when they serve a cached asset of a proxy repository
// without fetching it from the remote; a revalidation answered 304 by the
// remote also counts as a hit.
func CacheHit(format string, size int64) {
	cacheRequests.WithLabelValues(format, CacheResultHit).Inc()
	if size > 0 {
		cacheHitBytes.WithLabelValues(format).Add(float64(size))
	}
}

// CacheMiss records a proxy request of format that had to fetch the content
// from the remote (whether or not the remote had it).
func CacheMiss(format string) {
	cacheRequests.WithLabelValues(format, CacheResultMiss).Inc()
}

// CacheTally collects the proxy cache outcome of one client request so the
// request is counted once in the cache metrics, however many proxy members
// of a group it consulted: a miss when any remote had to be asked for the
// content, otherwise a hit when a proxy cache answered. Requests answered by
// hosted repositories alone are not counted.
//
//	ctx, tally := proxy.WithCacheTally(r.Context())
//	defer tally.Record(Name, r.Method)
//	... proxy.NoteCacheHit(ctx, size) / proxy.NoteCacheMiss(ctx) at decision points
type CacheTally struct {
	mu        sync.Mutex
	hit, miss bool
	size      int64
}

type cacheTallyKey struct{}

// WithCacheTally returns ctx carrying a fresh tally for one client request.
func WithCacheTally(ctx context.Context) (context.Context, *CacheTally) {
	t := &CacheTally{}
	return context.WithValue(ctx, cacheTallyKey{}, t), t
}

// NoteCacheHit notes that a proxy repository answered from its cache (fresh,
// revalidated with a 304, or stale because the remote failed) with size
// bytes. Without a tally in ctx it does nothing.
func NoteCacheHit(ctx context.Context, size int64) {
	if t, ok := ctx.Value(cacheTallyKey{}).(*CacheTally); ok {
		t.mu.Lock()
		t.hit = true
		t.size += size
		t.mu.Unlock()
	}
}

// NoteCacheMiss notes that a proxy repository asked its remote for the
// content. Without a tally in ctx it does nothing.
func NoteCacheMiss(ctx context.Context) {
	if t, ok := ctx.Value(cacheTallyKey{}).(*CacheTally); ok {
		t.mu.Lock()
		t.miss = true
		t.mu.Unlock()
	}
}

// Record adds the request to the cache metrics of format. A HEAD request
// served no bytes.
func (t *CacheTally) Record(format, method string) {
	t.mu.Lock()
	hit, miss, size := t.hit, t.miss, t.size
	t.mu.Unlock()
	switch {
	case miss:
		CacheMiss(format)
	case hit:
		if method == http.MethodHead {
			size = 0
		}
		CacheHit(format, size)
	}
}

// SetSize replaces the hit byte count with n, for responses derived from
// cached documents (a checksum computed from a cached jar serves the checksum,
// not the jar).
func (t *CacheTally) SetSize(n int64) {
	t.mu.Lock()
	t.size = n
	t.mu.Unlock()
}
