package observability

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ismoilovdevml/hangar/internal/version"
)

// TracingOptions configure SetupTracing (observability.tracing.*).
type TracingOptions struct {
	Enabled bool
	// Endpoint is the OTLP/HTTP collector URL, e.g. http://otel-collector:4318.
	// Without a path, /v1/traces is used. https:// uses TLS with the system
	// roots. Extra headers (collector auth) come from
	// OTEL_EXPORTER_OTLP_HEADERS / OTEL_EXPORTER_OTLP_TRACES_HEADERS.
	Endpoint string
	// SampleRatio is the share of new traces recorded, 0 to 1. Requests that
	// carry a sampled W3C traceparent are always recorded.
	SampleRatio float64
	// ServiceName is service.name on every span (default "hangar").
	ServiceName string
}

// ParseOTLPEndpoint validates an OTLP/HTTP endpoint and returns the traces
// URL: the path defaults to /v1/traces.
func ParseOTLPEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("tracing endpoint %q must be an http:// or https:// URL", redactEndpoint(raw))
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/traces"
	}
	return u.String(), nil
}

func redactEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

// SetupTracing installs the global OpenTelemetry tracer provider and W3C
// trace-context propagation when tracing is enabled, and returns a function
// that flushes and stops it. With tracing disabled it changes nothing and
// every span Hangar starts is a no-op.
func SetupTracing(ctx context.Context, o TracingOptions, log *slog.Logger) (shutdown func(context.Context) error, err error) {
	noop := func(context.Context) error { return nil }
	if !o.Enabled {
		return noop, nil
	}
	if log == nil {
		log = slog.Default()
	}
	endpoint, err := ParseOTLPEndpoint(o.Endpoint)
	if err != nil {
		return noop, err
	}
	if o.SampleRatio < 0 || o.SampleRatio > 1 {
		return noop, fmt.Errorf("tracing sample ratio %v must be between 0 and 1", o.SampleRatio)
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return noop, fmt.Errorf("otlp exporter: %w", err)
	}
	name := o.ServiceName
	if name == "" {
		name = "hangar"
	}
	attrs := []attribute.KeyValue{
		attribute.String("service.name", name),
		attribute.String("service.version", version.Get().Version),
	}
	if host, err := os.Hostname(); err == nil {
		attrs = append(attrs, attribute.String("host.name", host), attribute.String("service.instance.id", host))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...), resource.WithTelemetrySDK())
	if err != nil {
		return noop, fmt.Errorf("trace resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(o.SampleRatio))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	otel.SetErrorHandler(&throttledErrors{log: log, every: time.Minute})
	log.Info("tracing enabled", "endpoint", redactEndpoint(endpoint), "sample_ratio", o.SampleRatio, "service_name", name)
	return func(ctx context.Context) error {
		err := tp.Shutdown(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("flush traces: %w", err)
		}
		return err
	}, nil
}

// throttledErrors logs exporter errors at most once per interval, so an
// unreachable collector does not flood the log.
type throttledErrors struct {
	log   *slog.Logger
	every time.Duration

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func (t *throttledErrors) Handle(err error) {
	t.mu.Lock()
	now := time.Now()
	if now.Sub(t.last) < t.every {
		t.suppressed++
		t.mu.Unlock()
		return
	}
	n := t.suppressed
	t.last, t.suppressed = now, 0
	t.mu.Unlock()
	t.log.Warn("tracing: export failed", "error", err, "suppressed", n)
}
