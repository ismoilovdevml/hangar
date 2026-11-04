// Package authtest provides a Postgres-backed auth.Service for integration
// tests. Tests are skipped when HANGAR_TEST_DATABASE_URL is unset.
package authtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ismoilovdevml/hangar/internal/auth"
	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/db"
)

// EnvDatabaseURL names the PostgreSQL used by integration tests.
const EnvDatabaseURL = "HANGAR_TEST_DATABASE_URL"

// FastArgon2 keeps tests quick; never use it in production.
var FastArgon2 = auth.Argon2Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

// Migrations returns the base schema (00001-00006) and the auth ranges
// (00020-00029, 00040-00049, 00080-00089) plus the run-time settings
// (00090-00099), so these tests do not depend on unrelated migrations.
func Migrations(t testing.TB) fs.FS {
	t.Helper()
	src := db.Migrations()
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		t.Fatal(err)
	}
	out := fstest.MapFS{}
	for _, e := range entries {
		n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil || !authMigration(n) {
			continue
		}
		b, err := fs.ReadFile(src, e.Name())
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = &fstest.MapFile{Data: b}
	}
	return out
}

func authMigration(n int) bool {
	return n <= 6 || (n >= 20 && n <= 29) || (n >= 40 && n <= 49) || (n >= 80 && n <= 99)
}

// Pool connects to a fresh schema (dropped at cleanup). If migrate is true
// the base and auth migrations are applied.
func Pool(t testing.TB, migrate bool) *pgxpool.Pool {
	t.Helper()
	base := os.Getenv(EnvDatabaseURL)
	if base == "" {
		t.Skipf("%s not set; skipping database integration test", EnvDatabaseURL)
	}
	ctx := context.Background()
	admin, err := db.Open(ctx, base, db.PoolOptions{MaxConns: 2})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "hangar_auth_test_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := db.Open(ctx, u.String(), db.PoolOptions{MaxConns: 8})
	if err != nil {
		t.Fatalf("connect with schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close()
	})
	if migrate {
		p := Provider(t, pool)
		if _, err := p.Up(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return pool
}

// Provider is a goose provider over Migrations.
func Provider(t testing.TB, pool *pgxpool.Pool) *goose.Provider {
	t.Helper()
	sqlDB := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, Migrations(t))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Clock is a settable test clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts at the current time.
func NewClock() *Clock { return &Clock{t: time.Now().UTC().Truncate(time.Microsecond)} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Env is a service with its fakes.
type Env struct {
	Svc   *auth.Service
	Pool  *pgxpool.Pool
	Audit *coretest.AuditLog
	Repos *coretest.Repositories
	Clock *Clock
}

// Option adjusts the service options (e.g. to add a Directory).
type Option func(*auth.Options)

// WithDirectory sets the LDAP directory.
func WithDirectory(d auth.Directory) Option {
	return func(o *auth.Options) { o.Directory = d }
}

// New returns a service over a fresh migrated schema. cfg zero fields get
// defaults, with FastArgon2 unless cfg.Argon2 is set.
func New(t testing.TB, cfg auth.Config, opts ...Option) *Env {
	t.Helper()
	return NewOnPool(t, Pool(t, true), cfg, NewClock(), opts...)
}

// NewOnPool returns a service over an existing pool (e.g. to change config
// over the same data).
func NewOnPool(t testing.TB, pool *pgxpool.Pool, cfg auth.Config, clock *Clock, opts ...Option) *Env {
	t.Helper()
	if cfg.Argon2 == (auth.Argon2Params{}) {
		cfg.Argon2 = FastArgon2
	}
	e := &Env{Pool: pool, Audit: coretest.NewAuditLog(), Repos: coretest.NewRepositories(), Clock: clock}
	o := auth.Options{Config: cfg, Audit: e.Audit, Repos: e.Repos, Now: clock.Now}
	for _, opt := range opts {
		opt(&o)
	}
	svc, err := auth.New(pool, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	e.Svc = svc
	return e
}

// AdminPassword is the password of the bootstrap administrator.
const AdminPassword = "correct horse battery staple"

// Admin bootstraps "admin" and returns a context acting as them.
func (e *Env) Admin(t testing.TB) context.Context {
	t.Helper()
	ctx := context.Background()
	if _, err := e.Svc.EnsureBootstrapAdmin(ctx, "admin", AdminPassword); err != nil {
		t.Fatal(err)
	}
	p, err := e.Svc.Authenticate(ctx, core.Credentials{Username: "admin", Password: AdminPassword})
	if err != nil {
		t.Fatal(err)
	}
	return core.WithPrincipal(ctx, p)
}

// As returns a context acting as username, authenticated with password.
func (e *Env) As(t testing.TB, username, password string) context.Context {
	t.Helper()
	ctx := context.Background()
	p, err := e.Svc.Authenticate(ctx, core.Credentials{Username: username, Password: password})
	if err != nil {
		t.Fatalf("authenticate %s: %v", username, err)
	}
	return core.WithPrincipal(ctx, p)
}
