// Package db owns the PostgreSQL connection pool and the schema migrations.
//
// Migrations live in migrations/ as goose SQL files, are embedded in the
// binary, and are numbered with unique, ascending five-digit prefixes (gaps
// allowed). Never edit a
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

	"github.com/jackc/pgx/v5"
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
	// Tracer, if set, observes every statement (metrics and spans).
	Tracer pgx.QueryTracer
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
	if opts.Tracer != nil {
		cfg.ConnConfig.Tracer = opts.Tracer
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
//
// Up, Down, DownTo and HasPending run under a PostgreSQL session advisory
// lock (see migrationLockKeySQL), so nodes that start together with
// migrate_on_start, or a `hangar migrate up` next to starting nodes, never
// apply a migration twice: one process migrates, the others wait for it and
// then find nothing pending.
type Migrator struct {
	pool     *pgxpool.Pool
	db       *sql.DB
	provider *goose.Provider
}

// migrationLockKeySQL is the advisory lock id taken around migrations: a
// fixed name hashed with the current schema, so installations that share a
// PostgreSQL database in different schemas do not wait for each other. It
// needs session-level locks, so the connection must not go through a
// transaction-pooling proxy (PgBouncer pool_mode=transaction).
const migrationLockKeySQL = `hashtextextended('hangar.migrations:' || coalesce(current_schema(), ''), 0)`

// withLock runs fn holding the migration lock on a dedicated connection of
// the pool. It waits as long as ctx allows for another holder.
func (m *Migrator) withLock(ctx context.Context, log *slog.Logger, fn func() error) error {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+migrationLockKeySQL+`)`).Scan(&got); err != nil {
		conn.Release()
		return fmt.Errorf("migration lock: %w", err)
	}
	if !got {
		if log != nil {
			log.Info("waiting for the migration lock: another process is applying migrations")
		}
		start := time.Now()
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+migrationLockKeySQL+`)`); err != nil {
			// A cancelled wait leaves the session in an unknown state (the
			// lock may even have been granted): never return it to the pool.
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
			conn.Release()
			if ctxErr := ctx.Err(); ctxErr != nil {
				err = ctxErr
			}
			return fmt.Errorf("migration lock: %w", err)
		}
		if log != nil {
			log.Info("migration lock acquired", "waited_ms", time.Since(start).Milliseconds())
		}
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		var released bool
		uerr := conn.QueryRow(uctx, `SELECT pg_advisory_unlock(`+migrationLockKeySQL+`)`).Scan(&released)
		if uerr != nil || !released {
			// Ending the session releases every advisory lock it holds.
			_ = conn.Conn().Close(uctx)
		}
		conn.Release()
	}()
	return fn()
}

// NewMigrator builds a migrator over pool. Close it when done (the pool stays open).
// The pool needs at least two connections: the migration lock pins one.
func NewMigrator(pool *pgxpool.Pool) (*Migrator, error) {
	if n := pool.Config().MaxConns; n < 2 {
		return nil, fmt.Errorf("migrations need a pool of at least 2 connections (the lock holds one), have %d", n)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	// Feature branches take migration numbers from separate ranges, so a
	// release can bring a migration numbered below one an earlier release
	// already applied. Apply such migrations instead of refusing to start.
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, Migrations(), goose.WithAllowOutofOrder(true))
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	return &Migrator{pool: pool, db: sqlDB, provider: p}, nil
}

// Close releases the database/sql handle (not the underlying pool).
func (m *Migrator) Close() error { return m.db.Close() }

// Up applies all pending migrations and logs each one. If another process
// is migrating the same schema, Up waits for it first.
func (m *Migrator) Up(ctx context.Context, log *slog.Logger) error {
	return m.withLock(ctx, log, func() error {
		results, err := m.provider.Up(ctx)
		logResults(log, results)
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		return nil
	})
}

// Down rolls back the most recent migration.
func (m *Migrator) Down(ctx context.Context, log *slog.Logger) error {
	return m.withLock(ctx, log, func() error {
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
	})
}

// DownTo rolls back to version (0 = empty schema).
func (m *Migrator) DownTo(ctx context.Context, version int64, log *slog.Logger) error {
	return m.withLock(ctx, log, func() error {
		results, err := m.provider.DownTo(ctx, version)
		logResults(log, results)
		if err != nil {
			return fmt.Errorf("migrate down to %d: %w", version, err)
		}
		return nil
	})
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

// HasPending reports whether migrations are waiting to be applied. While
// another process is migrating it waits for that to finish, so a node that
// does not migrate itself starts once the migrating one is done instead of
// failing on a half-migrated schema.
func (m *Migrator) HasPending(ctx context.Context, log *slog.Logger) (bool, error) {
	var pending bool
	err := m.withLock(ctx, log, func() error {
		var err error
		pending, err = m.provider.HasPending(ctx)
		return err
	})
	return pending, err
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
