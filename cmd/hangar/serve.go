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
	"github.com/ismoilovdevml/hangar/internal/db"
	"github.com/ismoilovdevml/hangar/internal/observability"
	"github.com/ismoilovdevml/hangar/internal/server"
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

	t := cfg.Observability.Tracing
	stopTracing, err := observability.SetupTracing(ctx, observability.TracingOptions{
		Enabled: t.Enabled, Endpoint: t.Endpoint, SampleRatio: t.SampleRatio, ServiceName: t.ServiceName,
	}, log)
	if err != nil {
		return err
	}
	// Runs last: flushes the spans of the shutdown itself.
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stopTracing(sctx); err != nil {
			log.Warn("tracing: shutdown", "error", err)
		}
	}()

	metrics := observability.NewMetrics()
	pool, err := db.Open(ctx, cfg.Database.URL, db.PoolOptions{
		MaxConns: cfg.Database.MaxConns, MinConns: cfg.Database.MinConns, ConnLifetime: cfg.Database.ConnLifetime,
		Tracer: observability.NewQueryTracer(metrics),
	})
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := ensureSchema(ctx, pool, *migrate || cfg.Database.MigrateOnStart, log); err != nil {
		return err
	}

	a, err := build(ctx, cfg, pool, metrics, log, time.Now())
	if err != nil {
		return err
	}
	defer a.close()

	err = server.Run(ctx, server.Config{
		Addr:              cfg.Server.Addr,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
		ShutdownTimeout:   cfg.Server.ShutdownTimeout,
		MaxHeaderBytes:    cfg.Security.Limits.MaxHeaderBytes,
	}, a.router, a.health, log, nil)
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
