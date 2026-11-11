package observability

import (
	"net/http"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/version"
)

// Label values used when an input is outside its known, bounded set.
const (
	labelOther   = "other"
	labelUnknown = "unknown"
)

// Bucket layouts.
var (
	// httpBuckets cover API calls (ms) up to multi-GB transfers (minutes).
	httpBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300}
	// opBuckets cover blob store, database and upstream round trips.
	opBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}
	// jobBuckets cover jobs from sub-second purges to hour-long GC runs.
	jobBuckets = []float64{.1, .5, 1, 5, 15, 60, 300, 900, 1800, 3600, 7200}
)

// Metrics holds the process-wide Prometheus registry and Hangar's metrics.
// Packages may register their own collectors on Registry. Build one with
// NewMetrics; the zero value is not usable. Every method that records into
// Metrics accepts a nil receiver and then does nothing.
type Metrics struct {
	Registry *prometheus.Registry

	// HTTP server, labelled by chi route pattern (never the raw path).
	HTTPRequests     *prometheus.CounterVec   // method, route, code
	HTTPChallenges   *prometheus.CounterVec   // method, route
	HTTPDuration     *prometheus.HistogramVec // method, route
	HTTPInFlight     prometheus.Gauge
	HTTPRequestLen   *prometheus.CounterVec // route
	HTTPResponseLen  *prometheus.CounterVec // route
	FormatRequests   *prometheus.CounterVec // format, repo_type, op, status_class
	FormatDuration   *prometheus.HistogramVec
	FormatUploadLen  *prometheus.CounterVec // format, repo_type
	FormatDownloadLn *prometheus.CounterVec // format, repo_type

	BlobOps      *prometheus.CounterVec   // driver, op, result
	BlobDuration *prometheus.HistogramVec // driver, op
	BlobPutBytes *prometheus.CounterVec   // driver

	UpstreamFetches  *prometheus.CounterVec   // format, result
	UpstreamDuration *prometheus.HistogramVec // format

	DBQueryDuration *prometheus.HistogramVec // op
	DBQueryErrors   *prometheus.CounterVec   // op

	Jobs         *prometheus.CounterVec   // type, result
	JobDuration  *prometheus.HistogramVec // type
	JobQueueWait *prometheus.HistogramVec // type

	AuthFailures *prometheus.CounterVec // method, reason
	AuthLockouts prometheus.Counter

	formats  *boundedSet
	jobTypes *boundedSet

	mu        sync.Mutex
	knownJobs map[string]bool

	static []prometheus.Collector // registered by NewMetrics
}

// NewMetrics creates a registry with the Go runtime, process and build-info
// collectors and every Hangar metric that does not need a live dependency.
// Collectors over the database pool, job queue and so on are added with the
// Register* methods once those exist.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	info := version.Get()
	build := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "hangar_build_info",
		Help:        "Build information; value is always 1.",
		ConstLabels: prometheus.Labels{"version": info.Version, "commit": info.Commit, "goversion": info.GoVersion},
	})
	build.Set(1)
	m := &Metrics{
		Registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_http_requests_total",
			Help: "HTTP requests by method, route pattern and status code.",
		}, []string{"method", "route", "code"}),
		HTTPChallenges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_http_auth_challenges_total",
			Help: "401 answers to requests that presented no credentials at all (the challenge clients answer by retrying with credentials), by method and route pattern. They are also counted in hangar_http_requests_total with code 401.",
		}, []string{"method", "route"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hangar_http_request_duration_seconds",
			Help:    "HTTP request latency (until the handler returns) by method and route pattern.",
			Buckets: httpBuckets,
		}, []string{"method", "route"}),
		HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hangar_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
		HTTPRequestLen: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_http_request_bytes_total",
			Help: "Request body bytes read by handlers, by route pattern.",
		}, []string{"route"}),
		HTTPResponseLen: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_http_response_bytes_total",
			Help: "Bytes written in HTTP responses by route pattern.",
		}, []string{"route"}),
		FormatRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_format_requests_total",
			Help: "Package protocol requests by format, repository type, operation (read, write, delete, other) and status class.",
		}, []string{"format", "repo_type", "op", "status_class"}),
		FormatDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hangar_format_request_duration_seconds",
			Help:    "Package protocol request latency by format, repository type and operation.",
			Buckets: httpBuckets,
		}, []string{"format", "repo_type", "op"}),
		FormatUploadLen: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_format_upload_bytes_total",
			Help: "Bytes received in package uploads (write operations) by format and repository type.",
		}, []string{"format", "repo_type"}),
		FormatDownloadLn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_format_download_bytes_total",
			Help: "Bytes sent in successful package downloads (2xx reads) by format and repository type.",
		}, []string{"format", "repo_type"}),
		BlobOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_blobstore_operations_total",
			Help: "Blob store operations by driver, operation (put, get, stat, delete) and result (ok, not_found, error).",
		}, []string{"driver", "op", "result"}),
		BlobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hangar_blobstore_operation_duration_seconds",
			Help:    "Blob store operation latency by driver and operation (put includes streaming the content).",
			Buckets: opBuckets,
		}, []string{"driver", "op"}),
		BlobPutBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_blobstore_put_bytes_total",
			Help: "Bytes written to blob stores by driver (before deduplication).",
		}, []string{"driver"}),
		UpstreamFetches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_upstream_fetches_total",
			Help: "Proxy remote fetches by format and result (ok, not_modified, not_found, client_error, server_error, circuit_open, blocked, canceled, error).",
		}, []string{"format", "result"}),
		UpstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hangar_upstream_fetch_duration_seconds",
			Help:    "Proxy remote fetch latency to response headers by format.",
			Buckets: opBuckets,
		}, []string{"format"}),
		DBQueryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hangar_db_query_duration_seconds",
			Help:    "PostgreSQL statement latency by SQL operation (select, insert, update, delete, with, copy, batch, other).",
			Buckets: opBuckets,
		}, []string{"op"}),
		DBQueryErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_db_query_errors_total",
			Help: "PostgreSQL statements that failed (excluding cancelled requests) by SQL operation.",
		}, []string{"op"}),
		Jobs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_jobs_total",
			Help: "Background job executions on this node by type and result (success, retry, failed).",
		}, []string{"type", "result"}),
		JobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hangar_job_duration_seconds",
			Help:    "Background job execution time by type.",
			Buckets: jobBuckets,
		}, []string{"type"}),
		JobQueueWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hangar_job_queue_wait_seconds",
			Help:    "Time from enqueue to the start of a job's first attempt, by type.",
			Buckets: jobBuckets,
		}, []string{"type"}),
		AuthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_auth_failures_total",
			Help: "Failed authentications by method (login, basic, token) and reason.",
		}, []string{"method", "reason"}),
		AuthLockouts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "hangar_auth_lockouts_total",
			Help: "Accounts locked after repeated failed password attempts.",
		}),
		formats:   newBoundedSet(maxFormats),
		jobTypes:  newBoundedSet(maxJobTypes),
		knownJobs: map[string]bool{},
	}
	m.static = []prometheus.Collector{build,
		m.HTTPRequests, m.HTTPChallenges, m.HTTPDuration, m.HTTPInFlight, m.HTTPRequestLen, m.HTTPResponseLen,
		m.FormatRequests, m.FormatDuration, m.FormatUploadLen, m.FormatDownloadLn,
		m.BlobOps, m.BlobDuration, m.BlobPutBytes,
		m.UpstreamFetches, m.UpstreamDuration,
		m.DBQueryDuration, m.DBQueryErrors,
		m.Jobs, m.JobDuration, m.JobQueueWait,
		m.AuthFailures, m.AuthLockouts,
	}
	reg.MustRegister(m.static...)
	return m
}

// Cardinality caps for label values that come from configuration or code
// rather than from a closed list in this package.
const (
	maxFormats  = 32
	maxJobTypes = 64
	// maxStores and maxUpstreams cap the per-store and per-proxy-repository
	// gauges; administrators create those, so they are few in practice.
	maxStores    = 64
	maxUpstreams = 512
)

// boundedSet admits at most max distinct values; later new values map to
// "other", so a bug or misuse can never explode a metric's series count.
type boundedSet struct {
	mu   sync.Mutex
	max  int
	seen map[string]struct{}
}

func newBoundedSet(n int) *boundedSet { return &boundedSet{max: n, seen: map[string]struct{}{}} }

func (b *boundedSet) value(v string) string {
	if v == "" {
		return labelUnknown
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.seen[v]; ok {
		return v
	}
	if len(b.seen) >= b.max {
		return labelOther
	}
	b.seen[v] = struct{}{}
	return v
}

// methodLabel maps the request method onto the standard methods; anything
// else (clients may send arbitrary tokens) becomes "OTHER".
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return m
	}
	return "OTHER"
}

// codeLabel is the status code, or "other" outside 100-599.
func codeLabel(code int) string {
	if code < 100 || code > 599 {
		return labelOther
	}
	return strconv.Itoa(code)
}

// statusClass is "2xx", "4xx", ... or "other".
func statusClass(code int) string {
	if code < 100 || code > 599 {
		return labelOther
	}
	return strconv.Itoa(code/100) + "xx"
}

// formatOp classifies a package protocol request by method.
func formatOp(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead:
		return "read"
	case http.MethodPut, http.MethodPost, http.MethodPatch:
		return "write"
	case http.MethodDelete:
		return "delete"
	}
	return labelOther
}

// repoTypeLabel bounds the repository type.
func repoTypeLabel(t core.RepoType) string {
	switch t {
	case core.RepoHosted, core.RepoProxy, core.RepoGroup:
		return string(t)
	}
	return labelOther
}

// formatLabel bounds a format name.
func (m *Metrics) formatLabel(f string) string { return m.formats.value(f) }

// jobTypeLabel bounds a job type.
func (m *Metrics) jobTypeLabel(t string) string { return m.jobTypes.value(t) }
