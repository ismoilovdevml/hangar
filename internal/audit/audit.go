// Package audit stores the append-only audit log in PostgreSQL (table
// audit_events, range-partitioned by month) and serves queries and exports.
//
// Log implements core.AuditLog. Record never touches the database on the
// request path: events go into a bounded in-memory queue and a single writer
// goroutine inserts them in batches with COPY. When the queue is full, Record
// waits at most Options.EnqueueTimeout and then returns an error, so a slow
// database slows mutations down without blocking them forever. Close flushes
// everything still queued.
package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// ErrClosed is returned by Record after Close.
var ErrClosed = errors.New("audit log closed")

// ErrBackpressure is returned by Record when the queue stayed full for
// Options.EnqueueTimeout (the database is not keeping up).
var ErrBackpressure = errors.New("audit queue full")

// Options tune the asynchronous writer. Zero values select the defaults.
type Options struct {
	// QueueSize is the number of events buffered in memory (default 8192).
	QueueSize int
	// BatchSize is the maximum number of events per COPY (default 256).
	BatchSize int
	// FlushInterval bounds how long an event waits before it is written (default 250ms).
	FlushInterval time.Duration
	// EnqueueTimeout bounds how long Record waits for queue space (default 2s).
	EnqueueTimeout time.Duration
	// WriteTimeout bounds one batch insert attempt (default 10s).
	WriteTimeout time.Duration
	// Retries is how many times a failed batch is retried before its events
	// are written to the log instead (default 3).
	Retries int
	Logger  *slog.Logger
}

func (o *Options) defaults() {
	if o.QueueSize <= 0 {
		o.QueueSize = 8192
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 256
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 250 * time.Millisecond
	}
	if o.EnqueueTimeout <= 0 {
		o.EnqueueTimeout = 2 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 10 * time.Second
	}
	if o.Retries <= 0 {
		o.Retries = 3
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Log is the PostgreSQL audit log.
type Log struct {
	pool *pgxpool.Pool
	opts Options

	mu     sync.RWMutex // guards closed and sends on queue
	closed bool
	queue  chan core.AuditEvent
	flush  chan chan struct{}
	done   chan struct{}
}

var _ core.AuditLog = (*Log)(nil)

// New starts the writer goroutine. Call Close on shutdown to flush.
func New(pool *pgxpool.Pool, opts Options) *Log {
	opts.defaults()
	l := &Log{
		pool:  pool,
		opts:  opts,
		queue: make(chan core.AuditEvent, opts.QueueSize),
		flush: make(chan chan struct{}),
		done:  make(chan struct{}),
	}
	go l.run()
	return l
}

// Record implements core.AuditLog. At, Actor and IP default from ctx;
// Outcome defaults to success. It does not wait for the database.
func (l *Log) Record(ctx context.Context, e core.AuditEvent) error {
	if e.Action == "" {
		return fmt.Errorf("audit event without action: %w", core.ErrInvalid)
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.At = e.At.UTC()
	if e.Actor == "" {
		e.Actor = core.PrincipalFrom(ctx).Name
	}
	if e.IP == "" {
		e.IP = core.RequestInfoFrom(ctx).RemoteIP
	}
	switch e.Outcome {
	case "":
		e.Outcome = core.OutcomeSuccess
	case core.OutcomeSuccess, core.OutcomeDenied, core.OutcomeError:
	default:
		return fmt.Errorf("audit outcome %q: %w", e.Outcome, core.ErrInvalid)
	}

	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		return ErrClosed
	}
	select {
	case l.queue <- e:
		return nil
	default:
	}
	// The request context is deliberately ignored: a client that disconnects
	// after a mutation must not lose the record of it.
	t := time.NewTimer(l.opts.EnqueueTimeout)
	defer t.Stop()
	select {
	case l.queue <- e:
		return nil
	case <-t.C:
		l.opts.Logger.Error("audit queue full; event rejected", eventAttrs(e)...)
		return ErrBackpressure
	}
}

// Flush blocks until every event recorded before the call is written (or
// given up on), or ctx ends.
func (l *Log) Flush(ctx context.Context) error {
	req := make(chan struct{})
	select {
	case l.flush <- req:
	case <-l.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-req:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting events, writes everything queued and stops the
// writer. It waits until done or ctx ends.
func (l *Log) Close(ctx context.Context) error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.queue)
	}
	l.mu.Unlock()
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Log) run() {
	defer close(l.done)
	tick := time.NewTicker(l.opts.FlushInterval)
	defer tick.Stop()
	batch := make([]core.AuditEvent, 0, l.opts.BatchSize)
	write := func() {
		if len(batch) > 0 {
			l.write(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case e, ok := <-l.queue:
			if !ok {
				write()
				return
			}
			batch = append(batch, e)
			if len(batch) >= l.opts.BatchSize {
				write()
			}
		case <-tick.C:
			write()
		case req := <-l.flush:
		drain:
			for {
				select {
				case e, ok := <-l.queue:
					if !ok {
						break drain
					}
					batch = append(batch, e)
					if len(batch) >= l.opts.BatchSize {
						write()
					}
				default:
					break drain
				}
			}
			write()
			close(req)
		}
	}
}

var columns = []string{"at", "actor", "action", "target", "outcome", "ip", "details"}

// write inserts batch with retries; events that still fail are logged so
// they are not lost silently.
func (l *Log) write(batch []core.AuditEvent) {
	rows := make([][]any, len(batch))
	for i, e := range batch {
		details := e.Details
		ip, ok := parseIP(e.IP)
		if !ok && e.IP != "" {
			details = withRaw(details, e.IP)
		}
		if details == nil {
			details = map[string]any{}
		}
		var ipv any
		if ok {
			ipv = ip
		}
		rows[i] = []any{e.At, e.Actor, e.Action, e.Target, e.Outcome, ipv, details}
	}
	var err error
	backoff := 100 * time.Millisecond
	for attempt := 1; attempt <= l.opts.Retries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), l.opts.WriteTimeout)
		_, err = l.pool.CopyFrom(ctx, pgx.Identifier{"audit_events"}, columns, pgx.CopyFromRows(rows))
		cancel()
		if err == nil {
			return
		}
		l.opts.Logger.Warn("audit batch insert failed", "attempt", attempt, "events", len(batch), "error", err)
		if attempt < l.opts.Retries {
			time.Sleep(backoff)
			backoff *= 4
		}
	}
	for _, e := range batch {
		l.opts.Logger.Error("audit event not stored", append(eventAttrs(e), "error", err)...)
	}
}

func eventAttrs(e core.AuditEvent) []any {
	return []any{"audit_at", e.At, "audit_actor", e.Actor, "audit_action", e.Action,
		"audit_target", e.Target, "audit_outcome", e.Outcome, "audit_ip", e.IP, "audit_details", e.Details}
}

// parseIP accepts "1.2.3.4", "::1" and "host:port" forms.
func parseIP(s string) (netip.Prefix, bool) {
	if s == "" {
		return netip.Prefix{}, false
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		host, _, serr := net.SplitHostPort(s)
		if serr != nil {
			return netip.Prefix{}, false
		}
		if a, err = netip.ParseAddr(host); err != nil {
			return netip.Prefix{}, false
		}
	}
	a = a.Unmap().WithZone("")
	return netip.PrefixFrom(a, a.BitLen()), true
}

func withRaw(details map[string]any, ip string) map[string]any {
	out := make(map[string]any, len(details)+1)
	for k, v := range details {
		out[k] = v
	}
	out["ip_raw"] = ip
	return out
}

// EnsurePartitions creates the monthly partitions for the current month and
// monthsAhead following months (idempotent). The scheduler calls it daily.
func EnsurePartitions(ctx context.Context, pool *pgxpool.Pool, monthsAhead int) (int, error) {
	var n int
	if err := pool.QueryRow(ctx, "SELECT hangar_ensure_audit_partitions($1)", monthsAhead).Scan(&n); err != nil {
		return 0, fmt.Errorf("ensure audit partitions: %w", err)
	}
	return n, nil
}
