package observability

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/events"
)

// Job results.
const (
	jobSuccess = "success"
	jobRetry   = "retry"  // failed, will be retried
	jobFailed  = "failed" // failed on its last attempt; dropped
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

// RegisterJobQueue adds the job queue depth (messages per job type in the
// JetStream work queue: waiting plus running) and the NATS connection
// state. Depth is read from the stream on each scrape (bounded by timeout).
func (m *Metrics) RegisterJobQueue(js jetstream.JetStream, nc *nats.Conn, log *slog.Logger) error {
	if m == nil {
		return nil
	}
	return m.Registry.Register(&queueCollector{m: m, js: js, nc: nc, log: log, timeout: 2 * time.Second})
}

var (
	descQueueDepth = prometheus.NewDesc("hangar_job_queue_depth",
		"Jobs queued or running per type (messages in the JetStream work queue).", []string{"type"}, nil)
	descQueueUp = prometheus.NewDesc("hangar_job_queue_up",
		"1 if the job stream could be read on this scrape, else 0.", nil, nil)
	descNATSConnected = prometheus.NewDesc("hangar_nats_connected",
		"1 while the NATS connection is established, else 0.", nil, nil)
	descNATSReconnects = prometheus.NewDesc("hangar_nats_reconnects_total",
		"NATS reconnections since start.", nil, nil)
)

type queueCollector struct {
	m       *Metrics
	js      jetstream.JetStream
	nc      *nats.Conn
	log     *slog.Logger
	timeout time.Duration
}

func (c *queueCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descQueueDepth
	ch <- descQueueUp
	ch <- descNATSConnected
	ch <- descNATSReconnects
}

func (c *queueCollector) Collect(ch chan<- prometheus.Metric) {
	if c.nc != nil {
		connected := 0.0
		if c.nc.IsConnected() {
			connected = 1
		}
		ch <- prometheus.MustNewConstMetric(descNATSConnected, prometheus.GaugeValue, connected)
		ch <- prometheus.MustNewConstMetric(descNATSReconnects, prometheus.CounterValue, float64(c.nc.Stats().Reconnects))
	}
	depth, err := c.depth()
	if err != nil {
		if c.log != nil {
			c.log.Debug("job queue depth unavailable", "error", err)
		}
		ch <- prometheus.MustNewConstMetric(descQueueUp, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(descQueueUp, prometheus.GaugeValue, 1)
	c.m.mu.Lock()
	for t := range c.m.knownJobs {
		if _, ok := depth[t]; !ok {
			depth[t] = 0
		}
	}
	c.m.mu.Unlock()
	for t, n := range depth {
		ch <- prometheus.MustNewConstMetric(descQueueDepth, prometheus.GaugeValue, n, t)
	}
}

func (c *queueCollector) depth() (map[string]float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	stream, err := c.js.Stream(ctx, events.JobStream)
	if err != nil {
		return nil, err
	}
	info, err := stream.Info(ctx, jetstream.WithSubjectFilter(events.JobSubjectPrefix+">"))
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for subj, n := range info.State.Subjects {
		t := c.m.jobTypeLabel(strings.TrimPrefix(subj, events.JobSubjectPrefix))
		out[t] += float64(n)
	}
	return out, nil
}
