// Package scheduler enqueues periodic maintenance jobs on exactly one node.
//
// Every node runs a Scheduler, but only the leader enqueues: leadership is a
// PostgreSQL session-level advisory lock held on a dedicated connection.
// When the leader dies its connection closes, the lock is released and
// another node takes over within RetryEvery. Any node executes the enqueued
// jobs (the job queue distributes them), so the leader only decides when.
//
// Two kinds of work are scheduled. Entries are intervals kept in memory: a
// new leader runs each entry once after its Delay and then every Every, so
// their jobs must be idempotent and cheap to repeat. Tickers are called
// periodically while the node leads and decide themselves what is due; the
// task service is one, scheduling the cron schedules stored in the tasks
// table.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// LockKey is the advisory lock id of the scheduler leader.
const LockKey int64 = 0x68616e676172 // "hangar"

// DefaultRetryEvery is how often a follower tries to become the leader and
// the leader checks that it still holds the lock.
const DefaultRetryEvery = 30 * time.Second

// Entry is one periodic job.
type Entry struct {
	// Name identifies the entry in logs and in the job's dedup key.
	Name string
	// Every is the interval between runs (required).
	Every time.Duration
	// Delay is the wait after becoming leader before the first run.
	Delay time.Duration
	// Job is enqueued on every run (Type required). Key is filled from Name
	// and the run time when empty, so a run enqueued twice is stored once.
	Job core.Job
}

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
	Entries    []Entry
	Tickers    []Ticker
	RetryEvery time.Duration
	// LockKey overrides the advisory lock id (default LockKey).
	LockKey int64
	Logger  *slog.Logger
	// OnLeader, if set, is called with true on gaining and false on losing
	// leadership (tests, metrics).
	OnLeader func(bool)
}

// Scheduler is the leader-elected job scheduler.
type Scheduler struct {
	pool  *pgxpool.Pool
	queue core.JobQueue
	opts  Options
	log   *slog.Logger
}

// New returns a scheduler enqueueing into queue.
func New(pool *pgxpool.Pool, queue core.JobQueue, opts Options) (*Scheduler, error) {
	for _, e := range opts.Entries {
		if e.Name == "" || e.Every <= 0 || e.Job.Type == "" {
			return nil, fmt.Errorf("scheduler entry %q: name, every and job type are required: %w", e.Name, core.ErrInvalid)
		}
	}
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
	return &Scheduler{pool: pool, queue: queue, opts: opts, log: log}, nil
}

// Run campaigns for leadership and, while leader, enqueues the entries until
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
	s.log.Info("scheduler: this node is the leader", "entries", len(s.opts.Entries), "tickers", len(s.opts.Tickers))
	if s.opts.OnLeader != nil {
		s.opts.OnLeader(true)
		defer s.opts.OnLeader(false)
	}

	start := time.Now()
	next := make([]time.Time, len(s.opts.Entries))
	for i, e := range s.opts.Entries {
		next[i] = start.Add(e.Delay)
	}
	nextTick := make([]time.Time, len(s.opts.Tickers))
	for i := range nextTick {
		nextTick[i] = start
	}
	check := time.NewTicker(s.opts.RetryEvery)
	defer check.Stop()
	for {
		now := time.Now()
		wake := now.Add(s.opts.RetryEvery)
		for i, e := range s.opts.Entries {
			if !now.Before(next[i]) {
				s.enqueue(ctx, e, now)
				next[i] = now.Add(e.Every)
			}
			if next[i].Before(wake) {
				wake = next[i]
			}
		}
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

func (s *Scheduler) enqueue(ctx context.Context, e Entry, now time.Time) {
	j := e.Job
	if j.Key == "" {
		j.Key = e.Name + "@" + now.UTC().Truncate(e.Every).Format(time.RFC3339)
	}
	id, err := s.queue.Enqueue(ctx, j)
	if err != nil {
		s.log.Warn("scheduler: enqueue failed", "entry", e.Name, "type", j.Type, "error", err)
		return
	}
	s.log.Info("scheduler: job enqueued", "entry", e.Name, "type", j.Type, "job_id", id)
}
