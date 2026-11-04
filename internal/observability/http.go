package observability

import (
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// RouteUnmatched labels requests no route pattern matched.
const RouteUnmatched = "unmatched"

// HTTPOptions configure HTTPMiddleware.
type HTTPOptions struct {
	Logger *slog.Logger
	// Metrics, if set, receives the HTTP and format request metrics.
	Metrics *Metrics
	// AccessLogSampleRatio is the share of requests written to the access
	// log, from 0 to 1. Server errors (5xx) are always logged; 0 logs only
	// those, 1 logs every request.
	AccessLogSampleRatio float64
	// QuietPaths are logged at debug level (probes and scrapes).
	QuietPaths map[string]bool
}

// HTTPMiddleware observes every request: it starts a server span (joining
// a W3C traceparent sent by the client), attaches RequestTags, counts
// request and response bytes, records the HTTP metrics labelled by chi
// route pattern (never the raw path) and, for package protocol requests,
// the per-format metrics; then it writes one access log line. It must run
// inside a chi router so the route pattern is known after the handler.
func HTTPMiddleware(o HTTPOptions) func(http.Handler) http.Handler {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	m := o.Metrics
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, span := tracer().Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.request.method", r.Method),
					attribute.String("url.path", r.URL.Path),
					attribute.String("client.address", core.RequestInfoFrom(ctx).RemoteIP),
					attribute.String("user_agent.original", r.UserAgent()),
					attribute.String("hangar.request_id", core.RequestInfoFrom(ctx).RequestID),
				))
			defer span.End()
			ctx, tags := WithRequestTags(ctx)

			body := &countingBody{}
			req := r.WithContext(ctx)
			if r.Body != nil && r.Body != http.NoBody {
				body.rc = r.Body
				req.Body = body
			}
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			if m != nil {
				m.HTTPInFlight.Inc()
				defer m.HTTPInFlight.Dec()
			}
			next.ServeHTTP(ww, req)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			route := RouteUnmatched
			if rc := chi.RouteContext(req.Context()); rc != nil {
				if p := rc.RoutePattern(); p != "" {
					route = p
				}
			}
			dur := time.Since(start)
			bytesIn, bytesOut := body.n.Load(), int64(ww.BytesWritten())
			principal, repo, format, repoType := tags.snapshot()
			method := methodLabel(r.Method)

			span.SetName(method + " " + route)
			span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.response.status_code", status))
			if repo != "" {
				span.SetAttributes(attribute.String("hangar.repository", repo), attribute.String("hangar.format", format),
					attribute.String("hangar.repository_type", string(repoType)))
			}
			if principal != "" {
				span.SetAttributes(attribute.String("enduser.id", principal))
			}
			if status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, http.StatusText(status))
			}

			if m != nil {
				m.HTTPRequests.WithLabelValues(method, route, codeLabel(status)).Inc()
				m.HTTPDuration.WithLabelValues(method, route).Observe(dur.Seconds())
				m.HTTPRequestLen.WithLabelValues(route).Add(float64(bytesIn))
				m.HTTPResponseLen.WithLabelValues(route).Add(float64(bytesOut))
				if repo != "" {
					m.observeFormat(format, repoType, r.Method, status, dur, bytesIn, bytesOut)
				}
			}

			level := slog.LevelInfo
			switch {
			case status >= http.StatusInternalServerError && status != http.StatusNotImplemented:
				level = slog.LevelError
			case o.QuietPaths[r.URL.Path]:
				level = slog.LevelDebug
			}
			if level < slog.LevelError && !sampled(o.AccessLogSampleRatio) {
				return
			}
			info := core.RequestInfoFrom(ctx)
			log.LogAttrs(ctx, level, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", route),
				slog.Int("status", status),
				slog.Int64("bytes_in", bytesIn),
				slog.Int64("bytes_out", bytesOut),
				slog.Int64("duration_ms", dur.Milliseconds()),
				slog.String("remote_ip", info.RemoteIP),
				slog.String("user_agent", info.UserAgent),
			)
		})
	}
}

// observeFormat records one package protocol request.
func (m *Metrics) observeFormat(format string, repoType core.RepoType, method string, status int, dur time.Duration, in, out int64) {
	f, rt, op := m.formatLabel(format), repoTypeLabel(repoType), formatOp(method)
	m.FormatRequests.WithLabelValues(f, rt, op, statusClass(status)).Inc()
	m.FormatDuration.WithLabelValues(f, rt, op).Observe(dur.Seconds())
	if op == "write" && in > 0 {
		m.FormatUploadLen.WithLabelValues(f, rt).Add(float64(in))
	}
	if op == "read" && status >= 200 && status < 300 && out > 0 {
		m.FormatDownloadLn.WithLabelValues(f, rt).Add(float64(out))
	}
}

// sampled reports whether a request falls in the access log sample.
func sampled(ratio float64) bool {
	switch {
	case ratio >= 1:
		return true
	case ratio <= 0:
		return false
	}
	return rand.Float64() < ratio //nolint:gosec // sampling, not security
}

// countingBody counts the request body bytes the handler reads.
type countingBody struct {
	rc io.ReadCloser
	n  atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error { return b.rc.Close() }
