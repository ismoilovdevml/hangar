package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Job results.
const (
	jobSuccess = "success"
	jobRetry   = "retry"  // failed, will be retried
	jobFailed  = "failed" // failed on its last attempt; dead
)

// InstrumentJob wraps a job handler of jobType with metrics, a span and log
// correlation. maxAttempts is the queue's attempt limit, used to tell a
// retry from a permanent failure.
func InstrumentJob(m *Metrics, jobType string, maxAttempts int, h core.JobHandler) core.JobHandler {
	if m == nil {
		return h
	}
	label := m.jobTypeLabel(jobType)
	m.mu.Lock()
	m.knownJobs[label] = true
	m.mu.Unlock()
	return func(ctx context.Context, job core.Job) (err error) {
		ctx, span := tracer().Start(ctx, "job "+jobType, trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("hangar.job.type", jobType),
				attribute.String("hangar.job.id", job.ID),
				attribute.Int("hangar.job.attempt", job.Attempt),
			))
		start := time.Now()
		if job.Attempt <= 1 && !job.EnqueuedAt.IsZero() {
			m.JobQueueWait.WithLabelValues(label).Observe(max(start.Sub(job.EnqueuedAt), 0).Seconds())
		}
		defer func() {
			result := jobSuccess
			if err != nil {
				result = jobRetry
				if job.Attempt >= maxAttempts {
					result = jobFailed
				}
				span.RecordError(err)
				span.SetStatus(codes.Error, result)
			}
			m.Jobs.WithLabelValues(label, result).Inc()
			m.JobDuration.WithLabelValues(label).Observe(time.Since(start).Seconds())
			span.End()
		}()
		return h(ctx, job)
	}
}

// JobCounts are the jobs of one type per state.
type JobCounts struct {
	Queued, Running, Dead int64
}

// JobCounter reads the job queue: jobs per type and state, cluster-wide.
type JobCounter func(ctx context.Context) (map[string]JobCounts, error)

// EventListener is the state of a node's event bus connection.
type EventListener interface {
	Connected() bool
	Reconnects() uint64
}

// RegisterJobQueue adds the job queue depth (jobs per type waiting or
// running, and dead), read from the database on each scrape (bounded by a
// timeout), and the state of this node's event listener connection.
// Either argument may be nil.
func (m *Metrics) RegisterJobQueue(counts JobCounter, listener EventListener, log *slog.Logger) error {
	if m == nil {
		return nil
	}
	return m.Registry.Register(&queueCollector{m: m, counts: counts, listener: listener, log: log, timeout: 2 * time.Second})
}

var (
	descQueueDepth = prometheus.NewDesc("hangar_job_queue_depth",
		"Jobs waiting or running per type in the cluster-wide job queue.", []string{"type"}, nil)
	descQueueDead = prometheus.NewDesc("hangar_job_queue_dead",
		"Jobs per type that failed their last attempt, kept for inspection.", []string{"type"}, nil)
	descQueueUp = prometheus.NewDesc("hangar_job_queue_up",
		"1 if the job queue could be read on this scrape, else 0.", nil, nil)
	descListenerConnected = prometheus.NewDesc("hangar_event_listener_connected",
		"1 while this node's event listener connection to PostgreSQL is up, else 0.", nil, nil)
	descListenerReconnects = prometheus.NewDesc("hangar_event_listener_reconnects_total",
		"Reconnections of this node's event listener since start.", nil, nil)
)

type queueCollector struct {
	m        *Metrics
	counts   JobCounter
	listener EventListener
	log      *slog.Logger
	timeout  time.Duration
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descQueueDepth
	ch <- descQueueDead
	ch <- descQueueUp
	ch <- descListenerConnected
	ch <- descListenerReconnects
}

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	if c.listener != nil {
		connected := 0.0
		if c.listener.Connected() {
			connected = 1
		}
		ch <- prometheus.MustNewConstMetric(descListenerConnected, prometheus.GaugeValue, connected)
		ch <- prometheus.MustNewConstMetric(descListenerReconnects, prometheus.CounterValue, float64(c.listener.Reconnects()))
	}
	if c.counts == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	counts, err := c.counts(ctx)
	if err != nil {
		if c.log != nil {
			c.log.Debug("job queue depth unavailable", "error", err)
		}
		ch <- prometheus.MustNewConstMetric(descQueueUp, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(descQueueUp, prometheus.GaugeValue, 1)
	depth, dead := map[string]float64{}, map[string]float64{}
	c.m.mu.Lock()
	for t := range c.m.knownJobs {
		depth[t], dead[t] = 0, 0
	}
	c.m.mu.Unlock()
	for typ, n := range counts {
		t := c.m.jobTypeLabel(typ)
		depth[t] += float64(n.Queued + n.Running)
		dead[t] += float64(n.Dead)
	}
	for t, n := range depth {
		ch <- prometheus.MustNewConstMetric(descQueueDepth, prometheus.GaugeValue, n, t)
		ch <- prometheus.MustNewConstMetric(descQueueDead, prometheus.GaugeValue, dead[t], t)
	}
}
