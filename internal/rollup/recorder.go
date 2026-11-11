package rollup

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Defaults of Options.
const (
	DefaultInterval  = 15 * time.Second
	DefaultRetention = 62 * 24 * time.Hour // a 30-day range and the 30 days before it
	// maxPending bounds the minutes kept in memory while the database is
	// unreachable; older minutes are dropped first.
	maxPending = 24 * 60
	pruneEvery = time.Hour
)

// Options configure a Recorder.
type Options struct {
	// Started is when the process started: the counters were zero then.
	Started time.Time
	// Interval between reads (and writes); DefaultInterval when zero.
	Interval time.Duration
	// Retention of the minute rows; DefaultRetention when zero.
	Retention time.Duration
	Logger    *slog.Logger
}

// Recorder adds the increments of this process's counters to the minute rows
// of its instance. Run it once per process; Flush may also be called
// directly (tests, shutdown).
type Recorder struct {
	pool     *pgxpool.Pool
	g        prometheus.Gatherer
	opts     Options
	instance string
	now      func() time.Time

	mu      sync.Mutex
	prev    Counts
	prevAt  time.Time
	pending map[time.Time]Counts // by minute, not yet written
	covered bool                 // traffic_coverage row ensured
	pruned  time.Time
	failing bool
}

// NewRecorder reads the counters in g and writes to pool.
func NewRecorder(pool *pgxpool.Pool, g prometheus.Gatherer, opts Options) *Recorder {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	if opts.Started.IsZero() {
		opts.Started = time.Now()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Recorder{pool: pool, g: g, opts: opts, instance: newUUID(), now: time.Now,
		prevAt: opts.Started, pending: map[time.Time]Counts{}}
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Run flushes every Interval, aligned to the wall clock so that each interval
// falls inside one minute, until ctx ends; then it flushes a last time.
func (r *Recorder) Run(ctx context.Context) {
	for {
		now := r.now()
		wait := r.opts.Interval - time.Duration(now.UnixNano()%int64(r.opts.Interval))
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			r.flushLogged(fctx)
			cancel()
			return
		case <-t.C:
			r.flushLogged(ctx)
		}
	}
}

func (r *Recorder) flushLogged(ctx context.Context) {
	err := r.Flush(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case err != nil && !r.failing:
		r.failing = true
		r.opts.Logger.Warn("traffic statistics not written; keeping them in memory", "err", err)
	case err == nil && r.failing:
		r.failing = false
		r.opts.Logger.Info("traffic statistics written again")
	}
}

// Flush reads the counters, attributes the increments since the previous
// read to the minute that read happened in, and writes every pending minute
// in one transaction. On error the minutes stay pending for the next Flush.
func (r *Recorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, at := Read(r.g), r.now()
	if d := cur.Sub(r.prev); !d.IsZero() {
		m := r.prevAt.UTC().Truncate(time.Minute)
		r.pending[m] = r.pending[m].Add(d)
	}
	r.prev, r.prevAt = cur, at
	if err := r.write(ctx, at); err != nil {
		r.trim()
		return err
	}
	return nil
}

// trim drops the oldest pending minutes beyond maxPending.
func (r *Recorder) trim() {
	if len(r.pending) <= maxPending {
		return
	}
	keys := slices.SortedFunc(maps.Keys(r.pending), time.Time.Compare)
	for _, k := range keys[:len(keys)-maxPending] {
		delete(r.pending, k)
	}
}

const upsertTraffic = `
INSERT INTO traffic_minutes AS t (minute, instance, requests, downloads, cache_hits, cache_misses, cache_hit_bytes, latency,
	server_errors, denied, auth_challenges)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (minute, instance) DO UPDATE SET
	requests        = t.requests + excluded.requests,
	downloads       = t.downloads + excluded.downloads,
	server_errors   = t.server_errors + excluded.server_errors,
	denied          = t.denied + excluded.denied,
	auth_challenges = t.auth_challenges + excluded.auth_challenges,
	cache_hits      = t.cache_hits + excluded.cache_hits,
	cache_misses    = t.cache_misses + excluded.cache_misses,
	cache_hit_bytes = t.cache_hit_bytes + excluded.cache_hit_bytes,
	latency         = ARRAY(SELECT COALESCE(a, 0) + COALESCE(b, 0)
		FROM unnest(t.latency, excluded.latency) WITH ORDINALITY AS u(a, b, i) ORDER BY i)`

const upsertUpstream = `
INSERT INTO upstream_minutes AS t (minute, instance, repository, latency)
VALUES ($1, $2, $3, $4)
ON CONFLICT (minute, instance, repository) DO UPDATE SET
	latency = ARRAY(SELECT COALESCE(a, 0) + COALESCE(b, 0)
		FROM unnest(t.latency, excluded.latency) WITH ORDINALITY AS u(a, b, i) ORDER BY i)`

func (r *Recorder) write(ctx context.Context, now time.Time) error {
	prune := now.Sub(r.pruned) >= pruneEvery
	if len(r.pending) == 0 && r.covered && !prune {
		return nil
	}
	b := &pgx.Batch{}
	if !r.covered {
		b.Queue(`INSERT INTO traffic_coverage (since) VALUES ($1) ON CONFLICT (id) DO NOTHING`, r.opts.Started.UTC())
	}
	for m, c := range r.pending {
		if !c.trafficZero() {
			b.Queue(upsertTraffic, m, r.instance, c.Requests, c.Downloads, c.CacheHits, c.CacheMisses,
				c.CacheHitBytes, []int64(c.Latency), c.ServerErrors, c.Denied, c.Challenges)
		}
		for repo, h := range c.Upstreams {
			if h.Total() > 0 {
				b.Queue(upsertUpstream, m, r.instance, repo, []int64(h))
			}
		}
	}
	if prune {
		cut := now.Add(-r.opts.Retention).UTC()
		b.Queue(`DELETE FROM traffic_minutes WHERE minute < $1`, cut)
		b.Queue(`DELETE FROM upstream_minutes WHERE minute < $1`, cut)
	}
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.SendBatch(ctx, b).Close()
	})
	if err != nil {
		return fmt.Errorf("write traffic statistics: %w", err)
	}
	clear(r.pending)
	r.covered = true
	if prune {
		r.pruned = now
	}
	return nil
}
