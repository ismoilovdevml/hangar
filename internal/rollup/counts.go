// Package rollup persists the traffic numbers of the Overview screen in
// PostgreSQL: requests, downloads, request latency, proxy cache hits and
// misses, and proxy remote latency per repository.
//
// The numbers originate in this process's Prometheus counters. A Recorder
// reads them every few seconds and adds the increments to one row per minute
// and process (tables traffic_minutes and upstream_minutes), so they survive
// restarts and every node of a cluster contributes. A Store answers windowed
// queries by summing those rows across processes.
package rollup

import (
	"math"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// Prometheus series the Recorder reads. The HTTP ones are registered by
// observability.NewMetrics, the proxy ones by proxy.RegisterMetrics.
const (
	MetricHTTPRequests = "hangar_http_requests_total"
	MetricHTTPDuration = "hangar_http_request_duration_seconds"
	// MetricHTTPChallenges counts the 401 answers of MetricHTTPRequests to
	// requests that presented no credentials at all.
	MetricHTTPChallenges = "hangar_http_auth_challenges_total"
)

// Fixed histogram bounds (seconds) of the stored latency arrays. They match
// the Prometheus buckets of the source histograms; if those change, Read maps
// each stored bound to the largest source bucket not above it, so stored rows
// stay comparable across versions.
var (
	RequestBounds  = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300}
	UpstreamBounds = []float64{.025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}
)

// Histogram holds cumulative counts: element i counts the observations of at
// most bound i, and the last element counts every observation.
type Histogram []int64

// Total is the number of observations.
func (h Histogram) Total() int64 {
	if len(h) == 0 {
		return 0
	}
	return h[len(h)-1]
}

// Quantile estimates the q-quantile in seconds from cumulative counts at
// bounds, interpolating linearly inside the bucket. ok is false without
// observations. Above the largest bound it reports that bound.
func (h Histogram) Quantile(bounds []float64, q float64) (seconds float64, ok bool) {
	total := h.Total()
	if total <= 0 || len(h) != len(bounds)+1 {
		return 0, false
	}
	rank := q * float64(total)
	prevBound, prevCount := 0.0, 0.0
	for k, b := range bounds {
		c := float64(h[k])
		if c >= rank {
			v := prevBound
			if c > prevCount {
				v += (b - prevBound) * (rank - prevCount) / (c - prevCount)
			}
			return v, true
		}
		prevBound, prevCount = b, c
	}
	return bounds[len(bounds)-1], true
}

func (h Histogram) sub(o Histogram) Histogram {
	out := make(Histogram, len(h))
	for i, v := range h {
		var p int64
		if i < len(o) {
			p = o[i]
		}
		out[i] = max(v-p, 0)
	}
	return out
}

func (h Histogram) add(o Histogram) Histogram {
	if len(h) < len(o) {
		h = append(h, make(Histogram, len(o)-len(h))...)
	}
	for i, v := range o {
		h[i] += v
	}
	return h
}

// Counts are counter values, or the increments over an interval.
type Counts struct {
	Requests     int64
	Downloads    int64
	ServerErrors int64 // requests answered with a 5xx status
	Denied       int64 // requests refused with 401 or 403, challenges excluded
	// Challenges are 401 answers to requests without any credentials: the
	// authentication challenge clients answer by retrying with credentials.
	// They are part of Requests and not of Denied.
	Challenges    int64
	CacheHits     int64
	CacheMisses   int64
	CacheHitBytes int64
	Latency       Histogram            // at RequestBounds
	Upstreams     map[string]Histogram // at UpstreamBounds, by proxy repository name
}

// trafficZero reports whether the traffic_minutes part is all zero.
func (c Counts) trafficZero() bool {
	return c.Requests == 0 && c.Downloads == 0 && c.ServerErrors == 0 && c.Denied == 0 && c.Challenges == 0 &&
		c.CacheHits == 0 && c.CacheMisses == 0 &&
		c.CacheHitBytes == 0 && c.Latency.Total() == 0
}

// IsZero reports whether nothing happened.
func (c Counts) IsZero() bool {
	if !c.trafficZero() {
		return false
	}
	for _, h := range c.Upstreams {
		if h.Total() != 0 {
			return false
		}
	}
	return true
}

// Sub returns c - prev, never negative (a counter that went down restarted).
func (c Counts) Sub(prev Counts) Counts {
	d := func(a, b int64) int64 { return max(a-b, 0) }
	out := Counts{
		Requests: d(c.Requests, prev.Requests), Downloads: d(c.Downloads, prev.Downloads),
		ServerErrors: d(c.ServerErrors, prev.ServerErrors),
		Denied:       d(c.Denied, prev.Denied), Challenges: d(c.Challenges, prev.Challenges),
		CacheHits: d(c.CacheHits, prev.CacheHits), CacheMisses: d(c.CacheMisses, prev.CacheMisses),
		CacheHitBytes: d(c.CacheHitBytes, prev.CacheHitBytes), Latency: c.Latency.sub(prev.Latency),
	}
	for repo, h := range c.Upstreams {
		if s := h.sub(prev.Upstreams[repo]); s.Total() > 0 {
			if out.Upstreams == nil {
				out.Upstreams = map[string]Histogram{}
			}
			out.Upstreams[repo] = s
		}
	}
	return out
}

// Add returns c + o.
func (c Counts) Add(o Counts) Counts {
	out := Counts{
		Requests: c.Requests + o.Requests, Downloads: c.Downloads + o.Downloads,
		ServerErrors: c.ServerErrors + o.ServerErrors,
		Denied:       c.Denied + o.Denied, Challenges: c.Challenges + o.Challenges,
		CacheHits: c.CacheHits + o.CacheHits, CacheMisses: c.CacheMisses + o.CacheMisses,
		CacheHitBytes: c.CacheHitBytes + o.CacheHitBytes,
		Latency:       append(Histogram(nil), c.Latency...).add(o.Latency),
	}
	if len(c.Upstreams)+len(o.Upstreams) > 0 {
		out.Upstreams = map[string]Histogram{}
		for _, m := range []map[string]Histogram{c.Upstreams, o.Upstreams} {
			for repo, h := range m {
				out.Upstreams[repo] = append(Histogram(nil), out.Upstreams[repo]...).add(h)
			}
		}
	}
	return out
}

// isFormatRoute reports package client routes (content downloads).
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

func round(v float64) int64 { return int64(math.Round(v)) }

// histAccumulator sums Prometheus histograms and maps them onto fixed bounds.
type histAccumulator struct {
	count   float64
	buckets map[float64]float64 // upper bound -> cumulative count
}

func (a *histAccumulator) add(h *dto.Histogram) {
	if a.buckets == nil {
		a.buckets = map[float64]float64{}
	}
	a.count += float64(h.GetSampleCount())
	for _, b := range h.GetBucket() {
		a.buckets[b.GetUpperBound()] += float64(b.GetCumulativeCount())
	}
}

// at maps the accumulated buckets onto bounds: each bound takes the
// cumulative count of the largest source bucket not above it.
func (a *histAccumulator) at(bounds []float64) Histogram {
	out := make(Histogram, len(bounds)+1)
	for i, b := range bounds {
		best, cum := math.Inf(-1), 0.0
		for ub, c := range a.buckets {
			if ub <= b && ub > best {
				best, cum = ub, c
			}
		}
		out[i] = round(cum)
	}
	out[len(bounds)] = round(a.count)
	return out
}

// Read returns the current counter values in g. Series that are not
// registered, or have no observations yet, read as zero.
func Read(g prometheus.Gatherer) Counts {
	fams, _ := g.Gather() // partial results are still useful
	var c Counts
	var reqHist histAccumulator
	upstream := map[string]*histAccumulator{}
	for _, f := range fams {
		switch f.GetName() {
		case MetricHTTPRequests:
			for _, m := range f.GetMetric() {
				route := label(m, "route")
				if !countedRoute(route) {
					continue
				}
				n := round(m.GetCounter().GetValue())
				c.Requests += n
				code := label(m, "code")
				switch {
				case len(code) == 3 && code[0] == '5':
					c.ServerErrors += n
				case code == "401" || code == "403":
					c.Denied += n
				}
				if isFormatRoute(route) && label(m, "method") == "GET" && (code == "200" || code == "206") {
					c.Downloads += n
				}
			}
		case MetricHTTPChallenges:
			for _, m := range f.GetMetric() {
				if countedRoute(label(m, "route")) {
					c.Challenges += round(m.GetCounter().GetValue())
				}
			}
		case MetricHTTPDuration:
			for _, m := range f.GetMetric() {
				if countedRoute(label(m, "route")) {
					reqHist.add(m.GetHistogram())
				}
			}
		case proxy.MetricCacheRequests:
			for _, m := range f.GetMetric() {
				switch label(m, "result") {
				case proxy.CacheResultHit:
					c.CacheHits += round(m.GetCounter().GetValue())
				case proxy.CacheResultMiss:
					c.CacheMisses += round(m.GetCounter().GetValue())
				}
			}
		case proxy.MetricCacheHitBytes:
			for _, m := range f.GetMetric() {
				c.CacheHitBytes += round(m.GetCounter().GetValue())
			}
		case proxy.MetricUpstreamDuration:
			for _, m := range f.GetMetric() {
				repo := label(m, "repository")
				if upstream[repo] == nil {
					upstream[repo] = &histAccumulator{}
				}
				upstream[repo].add(m.GetHistogram())
			}
		}
	}
	// Challenges are 401 answers too; they are not refusals.
	c.Denied = max(c.Denied-c.Challenges, 0)
	c.Latency = reqHist.at(RequestBounds)
	for repo, a := range upstream {
		if c.Upstreams == nil {
			c.Upstreams = map[string]Histogram{}
		}
		c.Upstreams[repo] = a.at(UpstreamBounds)
	}
	return c
}
