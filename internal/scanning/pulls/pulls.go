// Package pulls records who downloads which package version: one counter per
// component, UTC day and client, kept in PostgreSQL (component_pulls) for
// the fix plan of the Vulnerabilities screen ("pulled 14 times by 3 builds in
// the last 7 days", "who pulls it").
//
// Downloads are only buffered in memory on the request path; a background
// loop adds them to the day rows every FlushInterval in one statement. Every
// node adds its own counts (pulls = pulls + n), so the numbers cover the
// whole cluster and survive restarts. Rows older than Retention are deleted.
package pulls

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Defaults of Options.
const (
	DefaultFlushInterval = 15 * time.Second
	DefaultRetention     = 90 * 24 * time.Hour
	DefaultMaxPending    = 50_000
	pruneEvery           = time.Hour
)

// Principal kinds stored in component_pulls.principal_kind.
const (
	KindUser           = "user"
	KindServiceAccount = "service_account"
	KindAnonymous      = "anonymous"
)

// Options configure a Recorder.
type Options struct {
	// FlushInterval between writes; DefaultFlushInterval when zero.
	FlushInterval time.Duration
	// Retention of the day rows; DefaultRetention when zero.
	Retention time.Duration
	// MaxPending bounds the buffered (component, day, client) counters. A
	// full buffer triggers an early flush; while the database is unreachable
	// new clients beyond it are dropped (and logged).
	MaxPending int
	Logger     *slog.Logger
	// Now is the clock (tests); time.Now when nil.
	Now func() time.Time
}

// Client is who pulled, as stored.
type Client struct {
	// Key identifies the client within a day: "user:alice",
	// "service_account:ci-publisher" or "anonymous:10.0.4.12 NuGet".
	Key       string
	Principal string // principal name; "" for anonymous pulls
	Kind      string // KindUser, KindServiceAccount or KindAnonymous
	Address   string
	Agent     string // AgentClass of the user agent
}

// ClientOf derives the client of a request from its principal and metadata.
func ClientOf(p *core.Principal, info core.RequestInfo) Client {
	c := Client{Address: info.RemoteIP, Agent: AgentClass(info.UserAgent), Kind: KindAnonymous}
	if !p.IsAnonymous() && p.Name != "" {
		c.Principal = p.Name
		c.Kind = KindUser
		if p.Kind == core.PrincipalServiceAccount {
			c.Kind = KindServiceAccount
		}
		c.Key = c.Kind + ":" + p.Name
		return c
	}
	c.Key = KindAnonymous + ":" + c.Address + " " + c.Agent
	return c
}

type key struct {
	component string
	day       time.Time
	client    string
}

type entry struct {
	Client
	n    int64
	last time.Time
}

// Recorder buffers downloads and writes them to component_pulls. It is safe
// for concurrent use.
type Recorder struct {
	pool *pgxpool.Pool
	opts Options

	mu      sync.Mutex
	pending map[key]*entry
	dropped int64
	pruned  time.Time
	closed  bool
	failing bool
	kick    chan struct{}
}

// New returns a Recorder writing to pool. Call Run to flush in the background.
func New(pool *pgxpool.Pool, opts Options) *Recorder {
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = DefaultFlushInterval
	}
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	if opts.MaxPending <= 0 {
		opts.MaxPending = DefaultMaxPending
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Recorder{pool: pool, opts: opts, pending: map[key]*entry{}, kick: make(chan struct{}, 1)}
}

// Retention is how long day rows are kept.
func (r *Recorder) Retention() time.Duration { return r.opts.Retention }

// Record notes one download of asset by the client of ctx. It never blocks
// on the database.
func (r *Recorder) Record(ctx context.Context, asset *core.Asset) {
	if asset == nil || !isUUID(asset.ComponentID) {
		return
	}
	c := ClientOf(core.PrincipalFrom(ctx), core.RequestInfoFrom(ctx))
	now := r.opts.Now().UTC()
	k := key{component: strings.ToLower(asset.ComponentID), day: now.Truncate(24 * time.Hour), client: c.Key}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	e := r.pending[k]
	if e == nil {
		if len(r.pending) >= r.opts.MaxPending*2 {
			r.dropped++
			r.mu.Unlock()
			return
		}
		e = &entry{Client: c}
		r.pending[k] = e
	}
	e.n++
	e.last = now
	e.Address, e.Agent = c.Address, c.Agent
	full := len(r.pending) >= r.opts.MaxPending
	r.mu.Unlock()
	if full {
		select {
		case r.kick <- struct{}{}:
		default:
		}
	}
}

// Run flushes every FlushInterval (or early when the buffer fills) until ctx
// ends, then flushes a last time and stops recording.
func (r *Recorder) Run(ctx context.Context) {
	t := time.NewTicker(r.opts.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.closed = true
			r.mu.Unlock()
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			r.flushLogged(fctx)
			cancel()
			return
		case <-t.C:
		case <-r.kick:
		}
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		r.flushLogged(fctx)
		cancel()
	}
}

func (r *Recorder) flushLogged(ctx context.Context) {
	err := r.Flush(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dropped > 0 {
		r.opts.Logger.Warn("package pull statistics buffer full; pulls of new clients dropped", "dropped", r.dropped)
		r.dropped = 0
	}
	switch {
	case err != nil && !r.failing:
		r.failing = true
		r.opts.Logger.Warn("package pull statistics not written; keeping them in memory", "error", err)
	case err == nil && r.failing:
		r.failing = false
		r.opts.Logger.Info("package pull statistics written again")
	}
}

const upsert = `
INSERT INTO component_pulls AS p (component_id, day, client, principal, principal_kind, address, agent, pulls, last_at)
SELECT d.component_id, d.day, d.client, NULLIF(d.principal, ''), d.kind, d.address, d.agent, d.n, d.at
  FROM unnest($1::uuid[], $2::date[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::bigint[], $9::timestamptz[])
       AS d(component_id, day, client, principal, kind, address, agent, n, at)
 WHERE EXISTS (SELECT 1 FROM components c WHERE c.id = d.component_id)
ON CONFLICT (component_id, day, client) DO UPDATE SET
	pulls   = p.pulls + excluded.pulls,
	last_at = greatest(p.last_at, excluded.last_at),
	address = CASE WHEN excluded.last_at >= p.last_at THEN excluded.address ELSE p.address END,
	agent   = CASE WHEN excluded.last_at >= p.last_at THEN excluded.agent ELSE p.agent END`

// Flush writes the buffered counters now, and deletes rows past the
// retention once an hour. On a database error the counters are kept for the
// next attempt; a batch the database rejects as invalid is dropped.
func (r *Recorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	batch := r.pending
	r.pending = map[key]*entry{}
	now := r.opts.Now().UTC()
	prune := now.Sub(r.pruned) >= pruneEvery
	r.mu.Unlock()

	b := &pgx.Batch{}
	if len(batch) > 0 {
		n := len(batch)
		comps, days, clients := make([]string, 0, n), make([]time.Time, 0, n), make([]string, 0, n)
		principals, kinds, addrs, agents := make([]string, 0, n), make([]string, 0, n), make([]string, 0, n), make([]string, 0, n)
		counts, lasts := make([]int64, 0, n), make([]time.Time, 0, n)
		for k, e := range batch {
			comps, days, clients = append(comps, k.component), append(days, k.day), append(clients, k.client)
			principals, kinds = append(principals, e.Principal), append(kinds, e.Kind)
			addrs, agents = append(addrs, e.Address), append(agents, e.Agent)
			counts, lasts = append(counts, e.n), append(lasts, e.last)
		}
		b.Queue(upsert, comps, days, clients, principals, kinds, addrs, agents, counts, lasts)
	}
	if prune {
		b.Queue(`DELETE FROM component_pulls WHERE day < $1::date`, now.Add(-r.opts.Retention).Truncate(24*time.Hour))
	}
	if b.Len() == 0 {
		return nil
	}
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error { return tx.SendBatch(ctx, b).Close() })
	if err == nil {
		if prune {
			r.mu.Lock()
			r.pruned = now
			r.mu.Unlock()
		}
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "23") {
		// A constraint the batch can never satisfy (e.g. a component deleted
		// between the check and the insert): retrying cannot help.
		return fmt.Errorf("write package pulls (%d counters dropped): %w", len(batch), err)
	}
	r.restore(batch)
	return fmt.Errorf("write package pulls: %w", err)
}

// restore puts an unwritten batch back, merging with what arrived meanwhile.
func (r *Recorder) restore(batch map[key]*entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range batch {
		if cur := r.pending[k]; cur != nil {
			cur.n += e.n
			if e.last.After(cur.last) {
				cur.last = e.last
			}
			continue
		}
		if len(r.pending) >= r.opts.MaxPending*2 {
			r.dropped++
			continue
		}
		r.pending[k] = e
	}
}

// Pending reports how many counters are buffered (tests, diagnostics).
func (r *Recorder) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// components wraps a ComponentService so that every download is also
// attributed to its client.
type components struct {
	core.ComponentService
	rec *Recorder
}

// Wrap returns cs, recording every RecordDownload in rec as well.
func Wrap(cs core.ComponentService, rec *Recorder) core.ComponentService {
	return &components{ComponentService: cs, rec: rec}
}

// RecordDownload implements core.ComponentService.
func (c *components) RecordDownload(ctx context.Context, asset *core.Asset) {
	c.ComponentService.RecordDownload(ctx, asset)
	c.rec.Record(ctx, asset)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}
