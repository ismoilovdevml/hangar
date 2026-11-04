// Package server assembles the HTTP router and runs it with graceful shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ismoilovdevml/hangar/internal/api"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/formats"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/observability"
	"github.com/ismoilovdevml/hangar/internal/webui"
)

// RouterOptions are the parts the router is built from.
type RouterOptions struct {
	Logger *slog.Logger
	// Metrics records the HTTP and format request metrics; nil records none.
	Metrics *observability.Metrics
	// MetricsPath serves Metrics.Registry for Prometheus; empty serves no
	// endpoint (metrics are still recorded, e.g. for the Overview screen).
	MetricsPath string
	// AccessLogSampleRatio is the share of requests written to the access
	// log (0 to 1); server errors are always logged.
	AccessLogSampleRatio float64
	Health               *health.Checker
	TrustedProxies       TrustedProxies
	// API is the fully wired REST API, mounted at /api/v1; nil serves none.
	API *api.API
	// Formats and Deps serve the package protocols; nil Formats serves none.
	Formats *formats.Registry
	Deps    core.Deps
	// FormatMiddleware wraps the format routes (/repository/..., /v2/...),
	// e.g. resolving browser sessions for downloads from the web UI.
	FormatMiddleware []func(http.Handler) http.Handler
	// Security configures the hardening middleware: security headers, CORS,
	// body limits and per-IP rate limits (security.go).
	Security Security
}

// NewRouter builds the full route tree:
//
//	/healthz /readyz /metrics      operations
//	/api/v1/...                    REST API (api/openapi.yaml)
//	/llms.txt                      index for automated clients
//	/repository/{repo}/... /v2/... format protocols
//	everything else                web UI (SPA)
func NewRouter(o RouterOptions) http.Handler {
	sec := newHardening(o.Security)
	r := chi.NewRouter()
	r.Use(RequestInfo(o.TrustedProxies))
	r.Use(observability.HTTPMiddleware(observability.HTTPOptions{
		Logger: o.Logger, Metrics: o.Metrics, AccessLogSampleRatio: o.AccessLogSampleRatio, QuietPaths: quietPaths(o.MetricsPath),
	}))
	r.Use(Recover(o.Logger))
	r.Use(sec.headers)
	r.Use(sec.cors)

	r.Group(func(ops chi.Router) {
		ops.Use(sec.bodyLimit(maxOtherBody))
		ops.Get("/healthz", health.Liveness)
		ops.Head("/healthz", health.Liveness)
		if o.Health != nil {
			ops.Get("/readyz", o.Health.Readiness)
		}
		if o.Metrics != nil && o.MetricsPath != "" {
			ops.Method(http.MethodGet, o.MetricsPath, promhttp.HandlerFor(o.Metrics.Registry, promhttp.HandlerOpts{Registry: o.Metrics.Registry}))
		}
	})

	if o.API != nil {
		r.Group(func(g chi.Router) {
			g.Use(apiHeaders, sec.apiRateLimit, sec.failedAuthLimit, sec.bodyLimit(o.Security.APIBodyBytes))
			o.API.Mount(g)
		})
		// The index for automated clients, at the conventional root path.
		r.Group(func(g chi.Router) {
			g.Use(apiHeaders, sec.apiRateLimit, sec.bodyLimit(maxOtherBody))
			g.Get("/llms.txt", o.API.LLMsTxt)
		})
	}

	if o.Formats != nil {
		r.Group(func(g chi.Router) {
			g.Use(packageHeaders, sec.failedAuthLimit, sec.bodyLimit(o.Security.UploadBodyBytes))
			for _, mw := range o.FormatMiddleware {
				g.Use(mw)
			}
			formats.Mount(g, o.Formats, o.Deps)
		})
	}

	ui := sec.uiHeaders(sec.bodyLimit(maxOtherBody)(webui.Handler()))
	r.NotFound(ui.ServeHTTP)
	return r
}

// quietPaths are logged at debug level: probes and scrapes.
func quietPaths(metricsPath string) map[string]bool {
	q := map[string]bool{"/healthz": true, "/readyz": true}
	if metricsPath != "" {
		q[metricsPath] = true
	}
	return q
}

// Config for Run.
type Config struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	// MaxHeaderBytes bounds the request line and headers (0 = 1 MiB).
	MaxHeaderBytes int
	// DrainDelay keeps serving (with /readyz failing) before shutdown starts,
	// so load balancers notice. 0 in tests and single-node setups.
	DrainDelay time.Duration
}

// Run listens on cfg.Addr and serves h until ctx is cancelled, then shuts
// down gracefully: readiness fails, in-flight requests get ShutdownTimeout.
// ready (may be nil) is called with the bound address once listening.
func Run(ctx context.Context, cfg Config, h http.Handler, hc *health.Checker, log *slog.Logger, ready func(net.Addr)) error {
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		// No ReadTimeout/WriteTimeout: multi-GB uploads and downloads stream
		// for as long as the client keeps them moving. Slow-header clients are
		// cut off by ReadHeaderTimeout, stalled bodies by the body idle
		// timeout of the router (Security.BodyIdleTimeout).
		ErrorLog:    slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Addr, err)
	}
	log.Info("http server listening", "addr", ln.Addr().String())
	if ready != nil {
		ready(ln.Addr())
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutting down", "drain_delay", cfg.DrainDelay.String(), "timeout", cfg.ShutdownTimeout.String())
	if hc != nil {
		hc.SetDraining()
	}
	if cfg.DrainDelay > 0 {
		time.Sleep(cfg.DrainDelay)
	}
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("http server stopped")
	return nil
}
