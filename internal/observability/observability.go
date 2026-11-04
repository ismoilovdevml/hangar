// Package observability is Hangar's telemetry: structured JSON logs with
// request correlation and secret scrubbing, Prometheus metrics with bounded
// label cardinality, and OpenTelemetry tracing exported over OTLP.
//
// The package owns the metric definitions (Metrics) and small decorators that
// add metrics and spans around the core interfaces without the instrumented
// packages knowing about it:
//
//   - HTTPMiddleware: request metrics, server spans and the access log
//   - TagRepositories / TagAuthenticator / TagPrincipal: per-request
//     repository, format and principal for logs, spans and format metrics
//   - InstrumentBlobStores, InstrumentUpstreams, InstrumentJob: latency,
//     outcome and spans of blob store operations, proxy fetches and jobs
//   - QueryTracer: pgx query spans and latency
//   - collectors for the database pool, job queue, NATS, audit queue,
//     readiness, proxy circuit breakers and blob store usage
//
// Every metric is documented in docs/operations/monitoring.md; a test keeps
// that list and the registered metrics in sync.
package observability

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName names Hangar's tracer.
const instrumentationName = "github.com/ismoilovdevml/hangar"

// tracer returns Hangar's tracer from the global provider (a no-op until
// SetupTracing installs one), so spans cost nothing when tracing is off.
func tracer() trace.Tracer { return otel.Tracer(instrumentationName) }
