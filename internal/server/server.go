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
}

// NewRouter builds the full route tree:
//
//	/healthz /readyz /metrics      operations
//	/api/v1/...                    REST API (api/openapi.yaml)
//	/repository/{repo}/... /v2/... format protocols
//	everything else                web UI (SPA)
func NewRouter(o RouterOptions) http.Handler {
	r := chi.NewRouter()
	r.Use(RequestInfo(o.TrustedProxies))
	r.Use(observability.HTTPMiddleware(observability.HTTPOptions{
		Logger: o.Logger, Metrics: o.Metrics, AccessLogSampleRatio: o.AccessLogSampleRatio, QuietPaths: quietPaths(o.MetricsPath),
	}))
	r.Use(Recover(o.Logger))

	r.Get("/healthz", health.Liveness)
	r.Head("/healthz", health.Liveness)
	if o.Health != nil {
		r.Get("/readyz", o.Health.Readiness)
	}
	if o.Metrics != nil && o.MetricsPath != "" {
		r.Method(http.MethodGet, o.MetricsPath, promhttp.HandlerFor(o.Metrics.Registry, promhttp.HandlerOpts{Registry: o.Metrics.Registry}))
	}

	if o.API != nil {
		o.API.Mount(r)
	}

	if o.Formats != nil {
		r.Group(func(g chi.Router) {
			for _, mw := range o.FormatMiddleware {
				g.Use(mw)
			}
			formats.Mount(g, o.Formats, o.Deps)
		})
	}

	ui := webui.Handler()
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
		// No ReadTimeout/WriteTimeout: multi-GB uploads and downloads stream
		// for as long as the client keeps them moving.
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
