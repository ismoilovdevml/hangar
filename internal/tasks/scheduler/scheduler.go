// Package scheduler runs periodic work on exactly one node.
//
// Every node runs a Scheduler, but only the leader calls its Tickers:
// leadership is a PostgreSQL session-level advisory lock held on a
// dedicated connection. When the leader dies its connection closes, the
// lock is released and another node takes over within RetryEvery. Tickers
// decide themselves what is due; the task service is one, enqueueing the
// runs of the cron schedules stored in the tasks table, which any node then
// executes (the job queue distributes them).
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LockKey is the advisory lock id of the scheduler leader.
const LockKey int64 = 0x68616e676172 // "hangar"

// DefaultRetryEvery is how often a follower tries to become the leader and
// the leader checks that it still holds the lock.
const DefaultRetryEvery = 30 * time.Second

// Ticker is periodic work done only by the leader. Tick returns when it
// wants to be called next; the scheduler calls it again no later than
// RetryEvery and no sooner than a second later. A zero time means "after
// RetryEvery". Errors are logged.
type Ticker interface {
	Tick(ctx context.Context, now time.Time) (time.Time, error)
}

// TickerFunc adapts a function to Ticker.
type TickerFunc func(ctx context.Context, now time.Time) (time.Time, error)

// Tick implements Ticker.
func (f TickerFunc) Tick(ctx context.Context, now time.Time) (time.Time, error) { return f(ctx, now) }

// minTickGap bounds how often a ticker runs.
const minTickGap = time.Second

// Options configure a Scheduler.
type Options struct {
	Tickers    []Ticker
	RetryEvery time.Duration
	// LockKey overrides the advisory lock id (default LockKey).
	LockKey int64
	Logger  *slog.Logger
	// OnLeader, if set, is called with true on gaining and false on losing
	// leadership (tests, metrics).
	OnLeader func(bool)
}

// Scheduler is the leader-elected scheduler.
type Scheduler struct {
	pool *pgxpool.Pool
	opts Options
	log  *slog.Logger
}

// New returns a scheduler that elects its leader on the database of pool.
func New(pool *pgxpool.Pool, opts Options) *Scheduler {
	if opts.LockKey == 0 {
		opts.LockKey = LockKey
	}
	if opts.RetryEvery <= 0 {
		opts.RetryEvery = DefaultRetryEvery
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{pool: pool, opts: opts, log: log}
}

// Run campaigns for leadership and, while leader, calls the tickers until
// ctx ends. It returns when ctx is done, releasing the lock.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		if err := s.lead(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("scheduler: leadership lost or unavailable", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.opts.RetryEvery):
		}
	}
}

var errNotLeader = errors.New("another node is the scheduler leader")

// lead holds the lock and schedules until the lock is lost or ctx ends.
func (s *Scheduler) lead(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// The lock lives as long as this session: never return the connection to
	// the pool while it may hold the lock.
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, s.opts.LockKey).Scan(&got); err != nil {
		return err
	}
	if !got {
		return nil // follower; try again later
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, s.opts.LockKey); err != nil {
			// Closing the session releases the lock as well.
			_ = conn.Conn().Close(uctx)
		}
	}()
	s.log.Info("scheduler: this node is the leader", "tickers", len(s.opts.Tickers))
	if s.opts.OnLeader != nil {
		s.opts.OnLeader(true)
		defer s.opts.OnLeader(false)
	}

	start := time.Now()
	nextTick := make([]time.Time, len(s.opts.Tickers))
	for i := range nextTick {
		nextTick[i] = start
	}
	check := time.NewTicker(s.opts.RetryEvery)
	defer check.Stop()
	for {
		now := time.Now()
		wake := now.Add(s.opts.RetryEvery)
		for i, t := range s.opts.Tickers {
			if !now.Before(nextTick[i]) {
				nextTick[i] = s.tick(ctx, t, now)
			}
			if nextTick[i].Before(wake) {
				wake = nextTick[i]
			}
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-check.C:
			timer.Stop()
			if err := conn.Ping(ctx); err != nil {
				return fmt.Errorf("%w: leader connection: %w", errNotLeader, err)
			}
		case <-timer.C:
		}
	}
}

// tick runs one ticker and returns when to run it again.
func (s *Scheduler) tick(ctx context.Context, t Ticker, now time.Time) time.Time {
	want, err := t.Tick(ctx, now)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("scheduler: ticker failed", "error", err)
	}
	after := time.Now()
	lo, hi := after.Add(minTickGap), after.Add(s.opts.RetryEvery)
	switch {
	case want.IsZero() || want.After(hi):
		return hi
	case want.Before(lo):
		return lo
	default:
		return want
	}
}
