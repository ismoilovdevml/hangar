package observability

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// maxStatementLen bounds the SQL text put on spans. Statements are
// parameterised; argument values are never recorded.
const maxStatementLen = 2048

// QueryTracer is a pgx tracer (set on pgx.ConnConfig.Tracer) that times
// every statement, batch and COPY into hangar_db_query_duration_seconds and
// opens a client span per statement. A nil *Metrics records spans only.
type QueryTracer struct {
	m *Metrics
}

// NewQueryTracer returns a tracer recording into m.
func NewQueryTracer(m *Metrics) *QueryTracer { return &QueryTracer{m: m} }

var (
	_ pgx.QueryTracer    = (*QueryTracer)(nil)
	_ pgx.BatchTracer    = (*QueryTracer)(nil)
	_ pgx.CopyFromTracer = (*QueryTracer)(nil)
)

type queryStart struct {
	at   time.Time
	op   string
	span trace.Span
}

type queryKey struct{}

func (t *QueryTracer) start(ctx context.Context, op, sql string) context.Context {
	attrs := []attribute.KeyValue{
		attribute.String("db.system.name", "postgresql"),
		attribute.String("db.operation.name", op),
	}
	if sql != "" {
		if len(sql) > maxStatementLen {
			sql = sql[:maxStatementLen]
		}
		attrs = append(attrs, attribute.String("db.query.text", strings.TrimSpace(sql)))
	}
	ctx, span := tracer().Start(ctx, "db "+op, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	return context.WithValue(ctx, queryKey{}, &queryStart{at: time.Now(), op: op, span: span})
}

func (t *QueryTracer) end(ctx context.Context, err error) {
	qs, ok := ctx.Value(queryKey{}).(*queryStart)
	if !ok {
		return
	}
	failed := err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, pgx.ErrNoRows)
	if t.m != nil {
		t.m.DBQueryDuration.WithLabelValues(qs.op).Observe(time.Since(qs.at).Seconds())
		if failed {
			t.m.DBQueryErrors.WithLabelValues(qs.op).Inc()
		}
	}
	if failed {
		qs.span.RecordError(err)
		qs.span.SetStatus(codes.Error, "query failed")
	}
	qs.span.End()
}

// TraceQueryStart implements pgx.QueryTracer.
func (t *QueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return t.start(ctx, sqlOp(data.SQL), data.SQL)
}

// TraceQueryEnd implements pgx.QueryTracer.
func (t *QueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	t.end(ctx, data.Err)
}

// TraceBatchStart implements pgx.BatchTracer.
func (t *QueryTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	ctx = t.start(ctx, "batch", "")
	if qs, ok := ctx.Value(queryKey{}).(*queryStart); ok && data.Batch != nil {
		qs.span.SetAttributes(attribute.Int("db.operation.batch.size", data.Batch.Len()))
	}
	return ctx
}

// TraceBatchQuery implements pgx.BatchTracer: failed statements of a batch
// count as errors of their own operation.
func (t *QueryTracer) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if t.m != nil && data.Err != nil && !errors.Is(data.Err, context.Canceled) {
		t.m.DBQueryErrors.WithLabelValues(sqlOp(data.SQL)).Inc()
	}
}

// TraceBatchEnd implements pgx.BatchTracer.
func (t *QueryTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	t.end(ctx, data.Err)
}

// TraceCopyFromStart implements pgx.CopyFromTracer.
func (t *QueryTracer) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	ctx = t.start(ctx, "copy", "")
	if qs, ok := ctx.Value(queryKey{}).(*queryStart); ok {
		qs.span.SetAttributes(attribute.String("db.collection.name", data.TableName.Sanitize()))
	}
	return ctx
}

// TraceCopyFromEnd implements pgx.CopyFromTracer.
func (t *QueryTracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	t.end(ctx, data.Err)
}

// sqlOp is the statement's leading keyword, from a closed set.
func sqlOp(sql string) string {
	s := strings.TrimLeft(sql, " \t\r\n(")
	for strings.HasPrefix(s, "--") || strings.HasPrefix(s, "/*") {
		if strings.HasPrefix(s, "--") {
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return labelOther
			}
			s = s[i+1:]
		} else {
			i := strings.Index(s, "*/")
			if i < 0 {
				return labelOther
			}
			s = s[i+2:]
		}
		s = strings.TrimLeft(s, " \t\r\n(")
	}
	end := strings.IndexFunc(s, func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') })
	if end < 0 {
		end = len(s)
	}
	switch kw := strings.ToLower(s[:end]); kw {
	case "select", "insert", "update", "delete", "with", "copy", "begin", "commit", "rollback":
		return kw
	}
	return labelOther
}

// RegisterDBPool adds gauges and counters over the pgx pool's statistics.
func (m *Metrics) RegisterDBPool(pool *pgxpool.Pool) error {
	if m == nil || pool == nil {
		return nil
	}
	return m.Registry.Register(poolCollector{pool})
}

var (
	descPoolConns = prometheus.NewDesc("hangar_db_pool_connections",
		"Database pool connections by state (acquired, idle, constructing).", []string{"state"}, nil)
	descPoolMax = prometheus.NewDesc("hangar_db_pool_max_connections",
		"Configured maximum size of the database pool.", nil, nil)
	descPoolAcquires = prometheus.NewDesc("hangar_db_pool_acquires_total",
		"Successful connection acquisitions from the pool.", nil, nil)
	descPoolEmptyAcquires = prometheus.NewDesc("hangar_db_pool_empty_acquires_total",
		"Acquisitions that had to wait because no idle connection was available.", nil, nil)
	descPoolCanceledAcquires = prometheus.NewDesc("hangar_db_pool_canceled_acquires_total",
		"Acquisitions abandoned because the caller's context ended.", nil, nil)
	descPoolAcquireWait = prometheus.NewDesc("hangar_db_pool_acquire_wait_seconds_total",
		"Total time spent waiting for a connection when none was idle.", nil, nil)
	descPoolNewConns = prometheus.NewDesc("hangar_db_pool_new_connections_total",
		"Connections opened by the pool.", nil, nil)
)

type poolCollector struct{ pool *pgxpool.Pool }

func (poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descPoolConns, descPoolMax, descPoolAcquires, descPoolEmptyAcquires,
		descPoolCanceledAcquires, descPoolAcquireWait, descPoolNewConns} {
		ch <- d
	}
}

func (c poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(descPoolConns, prometheus.GaugeValue, float64(s.AcquiredConns()), "acquired")
	ch <- prometheus.MustNewConstMetric(descPoolConns, prometheus.GaugeValue, float64(s.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(descPoolConns, prometheus.GaugeValue, float64(s.ConstructingConns()), "constructing")
	ch <- prometheus.MustNewConstMetric(descPoolMax, prometheus.GaugeValue, float64(s.MaxConns()))
	ch <- prometheus.MustNewConstMetric(descPoolAcquires, prometheus.CounterValue, float64(s.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(descPoolEmptyAcquires, prometheus.CounterValue, float64(s.EmptyAcquireCount()))
	ch <- prometheus.MustNewConstMetric(descPoolCanceledAcquires, prometheus.CounterValue, float64(s.CanceledAcquireCount()))
	ch <- prometheus.MustNewConstMetric(descPoolAcquireWait, prometheus.CounterValue, s.EmptyAcquireWaitTime().Seconds())
	ch <- prometheus.MustNewConstMetric(descPoolNewConns, prometheus.CounterValue, float64(s.NewConnsCount()))
}
