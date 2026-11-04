package observability

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/proxy"
)

// Upstream fetch results.
const (
	upstreamOK          = "ok"
	upstreamNotModified = "not_modified"
	upstreamNotFound    = "not_found"
	upstreamClientError = "client_error"
	upstreamServerError = "server_error"
	upstreamCircuitOpen = "circuit_open"
	upstreamBlocked     = "blocked"
	upstreamCanceled    = "canceled"
	upstreamError       = "error"
)

// InstrumentUpstreams wraps next so every proxy remote fetch records its
// latency to response headers and its result per format, and a client span.
// Metrics are labelled by format, not repository; per-repository health is
// in hangar_upstream_state.
func InstrumentUpstreams(next core.Upstreams, m *Metrics) core.Upstreams {
	if m == nil {
		return next
	}
	return instrumentedUpstreams{next: next, m: m}
}

type instrumentedUpstreams struct {
	next core.Upstreams
	m    *Metrics
}

func (u instrumentedUpstreams) ForRepository(ctx context.Context, repo *core.Repository) (core.Upstream, error) {
	up, err := u.next.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	return &instrumentedUpstream{next: up, m: u.m, repo: repo.Name, format: u.m.formatLabel(repo.Format)}, nil
}

type instrumentedUpstream struct {
	next   core.Upstream
	m      *Metrics
	repo   string
	format string
}

func (u *instrumentedUpstream) Fetch(ctx context.Context, req core.UpstreamRequest) (*core.UpstreamResponse, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	ctx, span := tracer().Start(ctx, "upstream.fetch", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("hangar.repository", u.repo),
			attribute.String("hangar.format", u.format),
			attribute.String("http.request.method", method),
			attribute.String("hangar.upstream.path", req.Path),
		))
	defer span.End()
	start := time.Now()
	resp, err := u.next.Fetch(ctx, req)
	result := upstreamResult(resp, err)
	u.m.UpstreamFetches.WithLabelValues(u.format, result).Inc()
	if result != upstreamCanceled && result != upstreamCircuitOpen {
		u.m.UpstreamDuration.WithLabelValues(u.format).Observe(time.Since(start).Seconds())
	}
	span.SetAttributes(attribute.String("hangar.upstream.result", result))
	if resp != nil {
		span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, result)
	} else if result == upstreamServerError {
		span.SetStatus(codes.Error, result)
	}
	return resp, err
}

func upstreamResult(resp *core.UpstreamResponse, err error) string {
	switch {
	case err != nil:
		switch {
		case errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests):
			return upstreamCircuitOpen
		case errors.Is(err, proxy.ErrBlocked):
			return upstreamBlocked
		case errors.Is(err, context.Canceled):
			return upstreamCanceled
		}
		return upstreamError
	case resp == nil:
		return upstreamError
	case resp.NotModified || resp.StatusCode == http.StatusNotModified:
		return upstreamNotModified
	case resp.StatusCode == http.StatusNotFound:
		return upstreamNotFound
	case resp.StatusCode >= 500:
		return upstreamServerError
	case resp.StatusCode >= 400:
		return upstreamClientError
	}
	return upstreamOK
}
