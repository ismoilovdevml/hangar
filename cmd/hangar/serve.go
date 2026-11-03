package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/config"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/db"
	"github.com/ismoilovdevml/hangar/internal/events"
	"github.com/ismoilovdevml/hangar/internal/formats/builtin"
	"github.com/ismoilovdevml/hangar/internal/health"
	"github.com/ismoilovdevml/hangar/internal/httpclient"
	"github.com/ismoilovdevml/hangar/internal/observability"
	"github.com/ismoilovdevml/hangar/internal/server"
	"github.com/ismoilovdevml/hangar/internal/storage"
	"github.com/ismoilovdevml/hangar/internal/version"
)

// loadConfig parses --config and returns the config and a logger for it.
func loadConfig(fs *flag.FlagSet, args []string, stderr io.Writer) (config.Config, *slog.Logger, error) {
	path := fs.String("config", "", "config file (default $HANGAR_CONFIG)")
	if err := fs.Parse(args); err != nil {
		return config.Config{}, nil, err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, observability.NewLogger(stderr, cfg.Log.Level, cfg.Log.Format), nil
}

func serveCmd(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	migrate := fs.Bool("migrate", false, "apply pending migrations before serving (also database.migrate_on_start)")
	cfg, log, err := loadConfig(fs, args, stderr)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	info := version.Get()
	log.Info("starting hangar", "version", info.Version, "commit", info.Commit, "addr", cfg.Server.Addr,
		"base_url", cfg.Server.BaseURL, "storage", cfg.Storage.Kind, "nats_embedded", cfg.NATS.Embedded)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	started := time.Now()

	// Database.
	pool, err := db.Open(ctx, cfg.Database.URL, db.PoolOptions{
		MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns, ConnLifetime: cfg.Database.ConnLifetime,
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := ensureSchema(ctx, pool, *migrate || cfg.Database.MigrateOnStart, log); err != nil {
		return err
	}

	// Blob storage.
	store, err := storage.Open(cfg.Storage)
	if err != nil {
		return err
	}

	// Event bus and job queue.
	rt, err := events.Open(ctx, events.Options{
		Embedded: cfg.NATS.Embedded, DataDir: cfg.NATS.DataDir, Listen: cfg.NATS.Listen, URL: cfg.NATS.URL,
	}, log)
	if err != nil {
		return err
	}
	defer rt.Close()
	queue, err := events.NewQueue(ctx, rt.JS, events.QueueOptions{}, log)
	if err != nil {
		return err
	}
	defer queue.Close()

	// Outbound HTTP for proxy repositories.
	userAgent := cfg.HTTP.UserAgent
	if userAgent == "" {
		userAgent = version.UserAgent()
	}
	clients, err := httpclient.New(httpclient.Options{
		Proxy: cfg.HTTP.Proxy, ProxyUsername: cfg.HTTP.ProxyUsername, ProxyPassword: cfg.HTTP.ProxyPassword,
		NoProxy: cfg.HTTP.NoProxy, UseEnvironment: cfg.HTTP.UseEnvironment, CAFile: cfg.HTTP.CAFile,
		ConnectTimeout: cfg.HTTP.ConnectTimeout, ReadTimeout: cfg.HTTP.ReadTimeout,
		IdleConnTimeout: cfg.HTTP.IdleConnTimeout, MaxIdleConns: cfg.HTTP.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.HTTP.MaxIdleConnsPerHost, UserAgent: userAgent,
	})
	if err != nil {
		return err
	}

	trusted, err := server.ParseTrustedProxies(cfg.Server.TrustedProxies)
	if err != nil {
		return err
	}
	reg, err := builtin.Registry()
	if err != nil {
		return err
	}

	hc := health.New(2 * time.Second)
	hc.Add("database", func(ctx context.Context) error { return pool.Ping(ctx) })
	hc.Add("blobstore", store.Ping)
	hc.Add("eventbus", rt.Ping)

	// Core services are wired here. Repositories, components, auth, authz,
	// audit and upstreams are not built yet; until then /repository routes
	// are disabled and the API answers 501 for them.
	deps := core.Deps{
		Blobs:       storage.Single{Store: store},
		HTTPClients: clients,
		Events:      events.NewBus(rt.Conn, log),
		Jobs:        queue,
		Logger:      log,
		BaseURL:     cfg.Server.BaseURL,
	}

	var metrics *observability.Metrics
	if cfg.Metrics.Enabled {
		metrics = observability.NewMetrics()
	}
	router := server.NewRouter(server.RouterOptions{
		Logger: log, Metrics: metrics, MetricsPath: cfg.Metrics.Path, Health: hc,
		TrustedProxies: trusted, Formats: reg, Deps: deps, Started: started, InstanceName: cfg.Server.InstanceName,
	})
	err = server.Run(ctx, server.Config{
		Addr:              cfg.Server.Addr,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
		ShutdownTimeout:   cfg.Server.ShutdownTimeout,
	}, router, hc, log, nil)
	if err != nil {
		return err
	}
	log.Info("hangar stopped")
	return nil
}

// ensureSchema migrates, or refuses to start on a schema that is behind.
func ensureSchema(ctx context.Context, pool *pgxpool.Pool, migrate bool, log *slog.Logger) error {
	m, err := db.NewMigrator(pool)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()
	if migrate {
		return m.Up(ctx, log)
	}
	pending, err := m.HasPending(ctx)
	if err != nil {
		return err
	}
	if pending {
		return errors.New("database schema is out of date: run `hangar migrate up` or start with --migrate")
	}
	return nil
}
