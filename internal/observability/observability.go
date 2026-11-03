// Package observability sets up structured logging and Prometheus metrics.
package observability

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/version"
)

// NewLogger returns a slog logger writing JSON (or text) to w at level. Every
// record logged with a context carrying core.RequestInfo gets request_id.
func NewLogger(w io.Writer, level, format string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(requestIDHandler{h})
}

type requestIDHandler struct{ slog.Handler }

func (h requestIDHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := core.RequestInfoFrom(ctx).RequestID; id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestIDHandler{h.Handler.WithAttrs(attrs)}
}

func (h requestIDHandler) WithGroup(name string) slog.Handler {
	return requestIDHandler{h.Handler.WithGroup(name)}
}

// Metrics holds the process-wide Prometheus registry and HTTP metrics.
// Packages register their own collectors on Registry.
type Metrics struct {
	Registry        *prometheus.Registry
	HTTPRequests    *prometheus.CounterVec
	HTTPDuration    *prometheus.HistogramVec
	HTTPInFlight    prometheus.Gauge
	HTTPResponseLen *prometheus.CounterVec
}

// NewMetrics creates a registry with Go runtime, process and build-info
// collectors plus the HTTP server metrics.
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
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "hangar_http_request_duration_seconds",
			Help:    "HTTP request latency by method and route pattern.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300},
		}, []string{"method", "route"}),
		HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "hangar_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
		HTTPResponseLen: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "hangar_http_response_bytes_total",
			Help: "Bytes written in HTTP responses by route pattern.",
		}, []string{"route"}),
	}
	reg.MustRegister(build, m.HTTPRequests, m.HTTPDuration, m.HTTPInFlight, m.HTTPResponseLen)
	return m
}
