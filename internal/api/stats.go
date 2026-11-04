package api

import (
	"context"
	"math"
	"time"

	"github.com/ismoilovdevml/hangar/internal/rollup"
)

// TrafficStats answers windowed traffic queries over every node (implemented
// by rollup.Store).
type TrafficStats interface {
	Overview(ctx context.Context, q rollup.OverviewQuery) (*rollup.Overview, error)
	UpstreamLatency(ctx context.Context, from, to time.Time) (map[string]rollup.Histogram, error)
}

func ptr[T any](v T) *T { return &v }

// requestNumbers is the requests block of OverviewMetrics.
type requestNumbers struct {
	Total         *int64     `json:"total"`
	TrendPct      *float64   `json:"trend_pct"`
	PeakTotal     *int64     `json:"peak_total"`
	P95MS         *float64   `json:"p95_ms"`
	CoverageStart *time.Time `json:"coverage_start"`
	ServerErrors  *int64     `json:"server_errors"`
	Denied        *int64     `json:"denied"`
}

// cacheNumbers is the cache block of OverviewMetrics.
type cacheNumbers struct {
	HitRatio   *float64 `json:"hit_ratio"`
	BytesSaved *int64   `json:"bytes_saved"`
	Hits       *int64   `json:"hits"`
	Misses     *int64   `json:"misses"`
}

func requestsFrom(o *rollup.Overview, start time.Time) requestNumbers {
	out := requestNumbers{Total: ptr(o.Requests), PeakTotal: ptr(o.PeakRequests),
		ServerErrors: ptr(o.ServerErrors), Denied: ptr(o.Denied)}
	if !o.CoverageStart.IsZero() {
		out.CoverageStart = ptr(maxTime(start, o.CoverageStart).UTC())
	}
	if p := o.PrevRequests; p != nil && *p > 0 {
		out.TrendPct = ptr(math.Round(float64(o.Requests-*p)/float64(*p)*1000) / 10)
	}
	if s, ok := o.Latency.Quantile(rollup.RequestBounds, 0.95); ok {
		out.P95MS = ptr(math.Round(s*1000*10) / 10)
	}
	return out
}

func cacheFrom(o *rollup.Overview) cacheNumbers {
	out := cacheNumbers{BytesSaved: ptr(o.CacheHitBytes), Hits: ptr(o.CacheHits), Misses: ptr(o.CacheMisses)}
	if n := o.CacheHits + o.CacheMisses; n > 0 {
		out.HitRatio = ptr(math.Round(float64(o.CacheHits)/float64(n)*10000) / 10000)
	}
	return out
}

// latencyMS is the q-quantile of an upstream latency histogram in whole
// milliseconds (at least 1), nil without requests.
func latencyMS(h rollup.Histogram, q float64) *int64 {
	s, ok := h.Quantile(rollup.UpstreamBounds, q)
	if !ok {
		return nil
	}
	return ptr(max(int64(math.Round(s*1000)), 1))
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
