// Package db owns the PostgreSQL connection pool and the schema migrations.
//
// Migrations live in migrations/ as goose SQL files, are embedded in the
// binary, and are numbered sequentially (00007_xxx.sql next). Never edit a
// migration that has been released; add a new one. Every migration has a
// working Down section, verified by TestMigrationsUpDown.
package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns the embedded migration files.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err) // embedded path is fixed at build time
	}
	return sub
}

// PoolOptions tune the pgx pool. Zero values keep pgx defaults.
type PoolOptions struct {
	MaxConns     int32
	MinConns     int32
	ConnLifetime time.Duration
}

// Open connects a pool and verifies the connection with a ping.
func Open(ctx context.Context, url string, opts PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.ConnLifetime > 0 {
		cfg.MaxConnLifetime = opts.ConnLifetime
	}
	if cfg.ConnConfig.RuntimeParams["application_name"] == "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = "hangar"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return pool, nil
}

// Migrator applies the embedded migrations.
type Migrator struct {
	db       *sql.DB
	provider *goose.Provider
}

// NewMigrator builds a migrator over pool. Close it when done (the pool stays open).
func NewMigrator(pool *pgxpool.Pool) (*Migrator, error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, Migrations())
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	return &Migrator{db: sqlDB, provider: p}, nil
}

// Close releases the database/sql handle (not the underlying pool).
func (m *Migrator) Close() error { return m.db.Close() }

// Up applies all pending migrations and logs each one.
func (m *Migrator) Up(ctx context.Context, log *slog.Logger) error {
	results, err := m.provider.Up(ctx)
	logResults(log, results)
	if err != nil {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Down rolls back the most recent migration.
func (m *Migrator) Down(ctx context.Context, log *slog.Logger) error {
	r, err := m.provider.Down(ctx)
	if r != nil {
		logResults(log, []*goose.MigrationResult{r})
	}
	if errors.Is(err, goose.ErrNoNextVersion) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

// DownTo rolls back to version (0 = empty schema).
func (m *Migrator) DownTo(ctx context.Context, version int64, log *slog.Logger) error {
	results, err := m.provider.DownTo(ctx, version)
	logResults(log, results)
	if err != nil {
		return fmt.Errorf("migrate down to %d: %w", version, err)
	}
	return nil
}

// MigrationStatus is one row of Status.
type MigrationStatus struct {
	Version   int64
	Name      string
	Applied   bool
	AppliedAt time.Time
}

// Status lists every known migration and whether it is applied.
func (m *Migrator) Status(ctx context.Context) ([]MigrationStatus, error) {
	st, err := m.provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("migration status: %w", err)
	}
	out := make([]MigrationStatus, 0, len(st))
	for _, s := range st {
		out = append(out, MigrationStatus{
			Version:   s.Source.Version,
			Name:      s.Source.Path,
			Applied:   s.State == goose.StateApplied,
			AppliedAt: s.AppliedAt,
		})
	}
	return out, nil
}

// Version returns the current schema version (0 if none applied).
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	return m.provider.GetDBVersion(ctx)
}

// HasPending reports whether migrations are waiting to be applied.
func (m *Migrator) HasPending(ctx context.Context) (bool, error) {
	return m.provider.HasPending(ctx)
}

func logResults(log *slog.Logger, results []*goose.MigrationResult) {
	if log == nil {
		return
	}
	for _, r := range results {
		if r == nil || r.Source == nil {
			continue
		}
		attrs := []any{"version", r.Source.Version, "file", r.Source.Path, "direction", r.Direction, "duration_ms", r.Duration.Milliseconds()}
		if r.Error != nil {
			log.Error("migration failed", append(attrs, "error", r.Error)...)
			continue
		}
		log.Info("migration applied", attrs...)
	}
}
