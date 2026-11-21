package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Scopes of SharedLimiter buckets in rate_limit_buckets.
const (
	ScopeLogin      = "login"       // auth.login_rate
	ScopeFailedAuth = "failed_auth" // security.rate_limit.failed_auth
)

// bucketStore is satisfied by *pgxpool.Pool.
type bucketStore interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SharedLimiter is a token bucket per key kept in PostgreSQL
// (rate_limit_buckets), so every node of a cluster draws from the same
// bucket: N nodes behind a load balancer allow what one node allows, not N
// times as much. It is meant for security controls whose events are rare
// (sign-in attempts, failed authentications), at one statement per event.
//
// When the database cannot be reached it falls back to a bucket in this
// node's memory with the same settings, so the limit degrades to per node
// instead of disappearing.
type SharedLimiter struct {
	db       bucketStore
	scope    string
	perSec   float64
	burst    float64
	now      func() time.Time
	fallback *RateLimiter
	log      *slog.Logger
}

// NewSharedLimiter returns a limiter over db's rate_limit_buckets for scope.
// A non-positive l.PerMinute returns nil, which allows everything. now is
// the clock (time.Now when nil); nodes' clocks only need to agree to within
// the refill time of one token.
func NewSharedLimiter(db bucketStore, scope string, l RateLimit, now func() time.Time, log *slog.Logger) *SharedLimiter {
	if l.PerMinute <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &SharedLimiter{db: db, scope: scope, perSec: l.PerMinute / 60, burst: float64(max(l.Burst, 1)), now: now,
		fallback: NewRateLimiter(l, now), log: log}
}

// Take takes one token for key. It reports whether there was one, the tokens
// left afterwards, and, when none was left, how long until one is.
func (l *SharedLimiter) Take(ctx context.Context, key string) (ok bool, left float64, wait time.Duration) {
	if l == nil {
		return true, 0, 0
	}
	now := l.now()
	ok, left, err := l.take(ctx, key, now)
	if err != nil {
		if ctx.Err() == nil {
			l.log.WarnContext(ctx, "rate limit: shared bucket unavailable, limiting on this node only",
				"scope", l.scope, "error", err)
		}
		ok, wait = l.fallback.Allow(key)
		return ok, 0, wait
	}
	if !ok {
		wait = time.Duration((1 - left) / l.perSec * float64(time.Second))
	}
	return ok, left, wait
}

// take runs the bucket update. The conditional DO UPDATE writes, and
// RETURNING yields a row, only when a token is available; otherwise the
// bucket is read to tell how long to wait. Elapsed time never counts
// negative, and updated_at never moves back, so a node whose clock lags
// cannot refill a bucket early.
func (l *SharedLimiter) take(ctx context.Context, key string, now time.Time) (bool, float64, error) {
	var left float64
	err := l.db.QueryRow(ctx, `
INSERT INTO rate_limit_buckets AS b (scope, key, tokens, updated_at, full_at)
VALUES ($1, $2, $3::float8 - 1, $4::timestamptz, $4::timestamptz + make_interval(secs => 1 / $5::float8))
ON CONFLICT (scope, key) DO UPDATE SET
    tokens = least($3::float8, b.tokens + greatest(extract(epoch FROM $4::timestamptz - b.updated_at), 0) * $5::float8) - 1,
    updated_at = greatest(b.updated_at, $4::timestamptz),
    full_at = greatest(b.updated_at, $4::timestamptz)
        + make_interval(secs => ($3::float8 - least($3::float8, b.tokens + greatest(extract(epoch FROM $4::timestamptz - b.updated_at), 0) * $5::float8) + 1) / $5::float8)
WHERE least($3::float8, b.tokens + greatest(extract(epoch FROM $4::timestamptz - b.updated_at), 0) * $5::float8) >= 1
RETURNING tokens`, l.scope, key, l.burst, now, l.perSec).Scan(&left)
	if err == nil {
		return true, left, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, 0, fmt.Errorf("take %s token: %w", l.scope, err)
	}
	err = l.db.QueryRow(ctx, `
SELECT least($3::float8, tokens + greatest(extract(epoch FROM $4::timestamptz - updated_at), 0) * $5::float8)
  FROM rate_limit_buckets WHERE scope = $1 AND key = $2`, l.scope, key, l.burst, now, l.perSec).Scan(&left)
	if err != nil {
		return false, 0, fmt.Errorf("read %s bucket: %w", l.scope, err)
	}
	return false, left, nil
}

// PurgeBuckets deletes the shared buckets of every scope that are full again
// (a full bucket and no row are the same). Safe to run on every node.
func PurgeBuckets(ctx context.Context, db bucketStore, now time.Time) error {
	_, err := db.Exec(ctx, `DELETE FROM rate_limit_buckets WHERE full_at < $1`, now)
	return dbErr(err, "purge rate limit buckets")
}
