package events

import (
	"context"
	"encoding/json"
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

// ErrClosed is returned when the bus or the queue is used after Close.
var ErrClosed = errors.New("events: closed")

// Defaults for QueueOptions.
const (
	// DefaultVisibility is how long a job may go without a heartbeat from
	// the node running it before another node takes it over.
	DefaultVisibility   = 10 * time.Minute
	DefaultShutdownWait = 15 * time.Second
	// DefaultPollInterval is how often an idle worker looks for jobs when
	// no notification woke it (notifications lost while the listener was
	// reconnecting, jobs whose node died).
	DefaultPollInterval = 5 * time.Second
	// DefaultDedupWindow is how long a finished job with a key keeps
	// another with the same type and key from being stored.
	DefaultDedupWindow = 10 * time.Minute
	// DefaultDeadRetention is how long jobs that failed their last attempt
	// are kept for inspection.
	DefaultDeadRetention = 7 * 24 * time.Hour
	defaultMaxAttempts   = 5
	defaultRetryBase     = 5 * time.Second
)

const (
	// queueTimeout bounds one bookkeeping statement (claim, heartbeat,
	// finish, enqueue).
	queueTimeout = 10 * time.Second
	// maintenanceEvery is how often finished and dead jobs are pruned.
	maintenanceEvery = time.Minute
	// maxRetryDelay caps the backoff between attempts.
	maxRetryDelay = 10 * time.Minute
)

// QueueOptions tune the job queue.
type QueueOptions struct {
	// Node names this node in the jobs it runs (jobs.locked_by, with a
	// per-process suffix).
	Node string
	// MaxAttempts before a job is dead. Default 5.
	MaxAttempts int
	// Visibility is the lease of a running job: how long it may go without
	// a sign of life from its node before another node runs it. While a
	// handler runs, the queue renews the lease every Visibility/3, so a job
	// outlives Visibility as long as its node does. Default
	// DefaultVisibility.
	Visibility time.Duration
	// Timeout bounds one run of a handler: its context deadline. Default
	// Visibility.
	Timeout time.Duration
	// ShutdownWait bounds how long Close waits for running handlers after
	// cancelling their contexts. Default DefaultShutdownWait.
	ShutdownWait time.Duration
	// PollInterval is the longest an idle worker sleeps without a
	// notification. Default DefaultPollInterval.
	PollInterval time.Duration
	// DedupWindow: see core.Job.Key. Default DefaultDedupWindow.
	DedupWindow time.Duration
	// DeadRetention: see DefaultDeadRetention.
	DeadRetention time.Duration
	// RetryBase is the delay before the second attempt; it doubles with
	// every further attempt, up to 10 minutes. Default 5s.
	RetryBase time.Duration
	// Instrument, if set, wraps every handler (metrics, tracing).
	Instrument func(jobType string, h core.JobHandler) core.JobHandler
}

func (o QueueOptions) withDefaults() QueueOptions {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = defaultMaxAttempts
	}
	if o.Visibility <= 0 {
		o.Visibility = DefaultVisibility
	}
	if o.Timeout <= 0 {
		o.Timeout = o.Visibility
	}
	if o.ShutdownWait <= 0 {
		o.ShutdownWait = DefaultShutdownWait
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.DedupWindow <= 0 {
		o.DedupWindow = DefaultDedupWindow
	}
	if o.DeadRetention <= 0 {
		o.DeadRetention = DefaultDeadRetention
	}
	if o.RetryBase <= 0 {
		o.RetryBase = defaultRetryBase
	}
	if o.Node == "" {
		o.Node = "hangar"
	}
	return o
}

// Queue is a core.JobQueue on the jobs table. Every node runs one worker
// per job type it handles, and a worker runs one job at a time, so the jobs
// queued behind a long one go to idle nodes. Workers claim jobs with
// FOR UPDATE SKIP LOCKED and hold them with a lease their node renews; a
// job whose node dies is claimed again once its lease expires. Failed jobs
// are retried with exponential backoff and end as dead after MaxAttempts.
// Workers are woken by a notification when a job is enqueued and poll as a
// fallback.
type Queue struct {
	pool    *pgxpool.Pool
	channel string
	opts    QueueOptions
	log     *slog.Logger
	// owner identifies this process in jobs.locked_by: a node restarted
	// with the same name must not mistake its predecessor's leases for its
	// own.
	owner string

	mu      sync.Mutex
	workers map[string]*worker
	closing bool
	stop    chan struct{}
	wg      sync.WaitGroup // workers and the maintenance loop

	// runCtx is the parent of every handler context; Close cancels it.
	runCtx    context.Context
	cancelRun context.CancelFunc
}

var _ core.JobQueue = (*Queue)(nil)

type worker struct {
	typ  string
	h    core.JobHandler
	wake chan struct{} // capacity 1: a pending wake-up
}

func newQueue(pool *pgxpool.Pool, channel string, opts QueueOptions, log *slog.Logger) *Queue {
	opts = opts.withDefaults()
	runCtx, cancelRun := context.WithCancel(context.Background())
	q := &Queue{pool: pool, channel: channel, opts: opts, log: log, owner: opts.Node + "/" + newID()[:12],
		workers: map[string]*worker{}, stop: make(chan struct{}), runCtx: runCtx, cancelRun: cancelRun}
	q.wg.Add(1)
	go q.maintain()
	return q
}

func validJobType(t string) error {
	if t == "" || strings.ContainsAny(t, " *>\t\r\n") || strings.HasPrefix(t, ".") || strings.HasSuffix(t, ".") {
		return fmt.Errorf("job type %q: %w", t, core.ErrInvalid)
	}
	return nil
}

// Enqueue implements core.JobQueue. Without Debounce, jobs with the same
// Type and Key are stored once while one waits or within DedupWindow of
// the last one's enqueue; with Debounce, see core.Job. It returns the id of
// the job that will do the work: j's, or the one it merged into.
func (q *Queue) Enqueue(ctx context.Context, j core.Job) (string, error) {
	if err := validJobType(j.Type); err != nil {
		return "", err
	}
	if j.Debounce > 0 && j.Key == "" {
		return "", fmt.Errorf("job %s: debounce needs a key: %w", j.Type, core.ErrInvalid)
	}
	var payload any // NULL for an empty payload
	if len(j.Payload) > 0 {
		if !json.Valid(j.Payload) {
			return "", fmt.Errorf("job %s: payload is not JSON: %w", j.Type, core.ErrInvalid)
		}
		payload = string(j.Payload)
	}
	if j.ID == "" {
		j.ID = newID()
	}
	var key any
	if j.Key != "" {
		key = j.Key
	}
	ctx, cancel := context.WithTimeout(ctx, queueTimeout)
	defer cancel()
	var (
		id       string
		inserted bool
		err      error
	)
	switch {
	case j.Debounce > 0:
		err = q.pool.QueryRow(ctx, `
INSERT INTO jobs (id, type, key, coalescing, payload, run_at)
VALUES ($1, $2, $3, true, $4::jsonb, now() + make_interval(secs => $5))
ON CONFLICT (type, key) WHERE state = 'queued' AND key IS NOT NULL
DO UPDATE SET coalescing = true
RETURNING id, xmax = 0`, j.ID, j.Type, key, payload, j.Debounce.Seconds()).Scan(&id, &inserted)
	case key != nil:
		err = q.pool.QueryRow(ctx, `
INSERT INTO jobs (id, type, key, payload)
SELECT $1, $2, $3, $4::jsonb
WHERE NOT EXISTS (SELECT 1 FROM jobs WHERE type = $2 AND key = $3
	AND (state = 'queued' OR enqueued_at > now() - make_interval(secs => $5)))
ON CONFLICT (type, key) WHERE state = 'queued' AND key IS NOT NULL DO NOTHING
RETURNING id, true`, j.ID, j.Type, key, payload, q.opts.DedupWindow.Seconds()).Scan(&id, &inserted)
		if errors.Is(err, pgx.ErrNoRows) {
			// Merged: report the job that does the work.
			err = q.pool.QueryRow(ctx, `SELECT id FROM jobs WHERE type = $1 AND key = $2
ORDER BY state = 'queued' DESC, enqueued_at DESC LIMIT 1`, j.Type, key).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				id, err = j.ID, nil // pruned in between; the work was done
			}
		}
	default:
		err = q.pool.QueryRow(ctx, `INSERT INTO jobs (id, type, payload) VALUES ($1, $2, $3::jsonb) RETURNING id, true`,
			j.ID, j.Type, payload).Scan(&id, &inserted)
	}
	if err != nil {
		return "", fmt.Errorf("enqueue %s: %w", j.Type, err)
	}
	if inserted {
		q.wakeType(j.Type)
		if _, err := q.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, q.channel, j.Type); err != nil {
			// Stored: the other nodes' workers find it when they poll.
			q.log.Debug("job queue: notify failed", "type", j.Type, "error", err)
		}
	}
	return id, nil
}

// Handle implements core.JobQueue: it starts this node's worker for jobType.
func (q *Queue) Handle(jobType string, h core.JobHandler) error {
	if err := validJobType(jobType); err != nil {
		return err
	}
	if q.opts.Instrument != nil {
		h = q.opts.Instrument(jobType, h)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closing {
		return fmt.Errorf("handler for %q: %w", jobType, ErrClosed)
	}
	if _, ok := q.workers[jobType]; ok {
		return fmt.Errorf("handler for %q: %w", jobType, core.ErrAlreadyExists)
	}
	w := &worker{typ: jobType, h: h, wake: make(chan struct{}, 1)}
	q.workers[jobType] = w
	q.wg.Add(1)
	go q.work(w)
	return nil
}

// notified handles a notification on the job channel: its payload is the
// type of a job that was enqueued.
func (q *Queue) notified(jobType string) { q.wakeType(jobType) }

func (q *Queue) wakeType(jobType string) {
	q.mu.Lock()
	w := q.workers[jobType]
	q.mu.Unlock()
	if w != nil {
		w.signal()
	}
}

// wakeAll wakes every worker: notifications may have been missed.
func (q *Queue) wakeAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, w := range q.workers {
		w.signal()
	}
}

func (w *worker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// claimed is a job this node holds the lease of.
type claimed struct {
	job      core.Job
	key      string
	coalesce bool
}

func (q *Queue) work(w *worker) {
	defer q.wg.Done()
	for {
		select {
		case <-q.stop:
			return
		default:
		}
		c, err := q.claim(w.typ)
		if err != nil {
			q.log.Warn("job queue: claim failed", "type", w.typ, "error", err)
			q.sleep(w, q.opts.PollInterval)
			continue
		}
		if c != nil {
			q.run(w, c)
			continue
		}
		q.sleep(w, q.idleWait(w.typ))
	}
}

// sleep waits for d, a wake-up of w, or Close.
func (q *Queue) sleep(w *worker, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-q.stop:
	case <-w.wake:
	case <-t.C:
	}
}

// idleWait is how long a worker without a job may sleep: until the next
// delayed job is due or a lease expires, at most PollInterval.
func (q *Queue) idleWait(jobType string) time.Duration {
	ctx, cancel := context.WithTimeout(context.Background(), queueTimeout)
	defer cancel()
	var next *time.Duration
	var secs *float64
	err := q.pool.QueryRow(ctx, `
SELECT extract(epoch FROM least(
	(SELECT min(run_at) FROM jobs WHERE type = $1 AND state = 'queued'),
	(SELECT min(locked_until) FROM jobs WHERE type = $1 AND state = 'running')) - now())::float8`, jobType).Scan(&secs)
	if err == nil && secs != nil {
		d := time.Duration(*secs*float64(time.Second)) + 10*time.Millisecond
		next = &d
	}
	if next == nil || *next > q.opts.PollInterval {
		return q.opts.PollInterval
	}
	return max(*next, 10*time.Millisecond)
}

// claim takes the next due job of jobType: a queued one, or a running one
// whose lease expired. A job is not taken while another job with the same
// key runs, so coalesced work never runs twice at once.
func (q *Queue) claim(jobType string) (*claimed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queueTimeout)
	defer cancel()
	var (
		c       claimed
		key     *string
		payload []byte
	)
	err := q.pool.QueryRow(ctx, `
UPDATE jobs SET state = 'running', attempts = attempts + 1, locked_by = $2,
	locked_until = now() + make_interval(secs => $3), started_at = now()
WHERE id = (
	SELECT j.id FROM jobs j
	WHERE j.type = $1
		AND ((j.state = 'queued' AND j.run_at <= now()) OR (j.state = 'running' AND j.locked_until < now()))
		AND (j.key IS NULL OR NOT EXISTS (
			SELECT 1 FROM jobs r WHERE r.type = j.type AND r.key = j.key AND r.id <> j.id
				AND r.state = 'running' AND r.locked_until >= now()))
	ORDER BY j.run_at, j.enqueued_at
	LIMIT 1
	FOR UPDATE SKIP LOCKED)
RETURNING id, key, coalescing, payload, attempts, enqueued_at`, jobType, q.owner, q.opts.Visibility.Seconds()).
		Scan(&c.job.ID, &key, &c.coalesce, &payload, &c.job.Attempt, &c.job.EnqueuedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.job.Type = jobType
	if key != nil {
		c.key, c.job.Key = *key, *key
	}
	if len(payload) > 0 {
		c.job.Payload = json.RawMessage(payload)
	}
	return &c, nil
}

// run executes one claimed job and records its outcome.
func (q *Queue) run(w *worker, c *claimed) {
	j := c.job
	if j.Attempt > q.opts.MaxAttempts {
		// Its node died on every attempt (the lease expired each time).
		q.log.Error("job dead: its node stopped during every attempt", "job_id", j.ID, "type", j.Type, "attempts", j.Attempt-1)
		q.finish(c, "dead", "lease expired on every attempt", 0)
		return
	}
	ctx, cancel := context.WithTimeout(q.runCtx, q.opts.Timeout)
	defer cancel()
	lost, stopBeat := q.heartbeat(c, cancel)
	err := safeRun(ctx, w.h, j)
	stopBeat()
	switch {
	case lost():
		q.log.Warn("job lease lost: another node took it over; this run's outcome is discarded", "job_id", j.ID, "type", j.Type)
	case err == nil:
		q.finish(c, "done", "", 0)
	case q.runCtx.Err() != nil:
		// Interrupted by Close: hand it back without counting the attempt.
		q.log.Info("job interrupted by shutdown, returned to the queue", "job_id", j.ID, "type", j.Type, "error", err)
		q.finish(c, "returned", err.Error(), 0)
	case j.Attempt >= q.opts.MaxAttempts:
		q.log.Error("job failed permanently", "job_id", j.ID, "type", j.Type, "attempt", j.Attempt, "error", err)
		q.finish(c, "dead", err.Error(), 0)
	default:
		q.log.Warn("job failed, will retry", "job_id", j.ID, "type", j.Type, "attempt", j.Attempt, "error", err)
		q.finish(c, "retry", err.Error(), retryDelay(q.opts.RetryBase, j.Attempt))
	}
}

// heartbeat renews the lease of c every Visibility/3 until stopped. When
// the lease is gone (another node took the job over after this node was
// cut off for longer than Visibility) it cancels the run. lost reports
// that; the returned stop function ends the heartbeat and waits for it.
func (q *Queue) heartbeat(c *claimed, cancelRun context.CancelFunc) (lost func() bool, stop func()) {
	var (
		mu     sync.Mutex
		isLost bool
	)
	done := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(q.opts.Visibility / 3)
		defer t.Stop()
		for {
			select {
			case <-quit:
				return
			case <-t.C:
			}
			ctx, cancel := context.WithTimeout(context.Background(), queueTimeout)
			tag, err := q.pool.Exec(ctx, `UPDATE jobs SET locked_until = now() + make_interval(secs => $3)
WHERE id = $1 AND locked_by = $2 AND state = 'running'`, c.job.ID, q.owner, q.opts.Visibility.Seconds())
			cancel()
			switch {
			case err != nil:
				q.log.Warn("job heartbeat failed; the job may be taken over if this persists", "job_id", c.job.ID, "type", c.job.Type, "error", err)
			case tag.RowsAffected() == 0:
				mu.Lock()
				isLost = true
				mu.Unlock()
				cancelRun()
				return
			}
		}
	}()
	lost = func() bool {
		mu.Lock()
		defer mu.Unlock()
		return isLost
	}
	return lost, func() {
		close(quit)
		<-done
	}
}

// finish records the outcome of a run: "done", "retry" (after delay),
// "returned" (to the queue at once, attempt not counted) or "dead". It
// runs after Close cancelled the handlers, so it has its own deadline.
func (q *Queue) finish(c *claimed, outcome, errMsg string, delay time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), queueTimeout)
	defer cancel()
	id := c.job.ID
	var err error
	switch outcome {
	case "done":
		if c.key == "" || c.coalesce {
			_, err = q.pool.Exec(ctx, `DELETE FROM jobs WHERE id = $1 AND locked_by = $2`, id, q.owner)
		} else {
			// Kept for the deduplication window.
			_, err = q.pool.Exec(ctx, `UPDATE jobs SET state = 'done', finished_at = now(), locked_by = NULL,
	locked_until = NULL, last_error = '' WHERE id = $1 AND locked_by = $2`, id, q.owner)
		}
	case "dead":
		_, err = q.pool.Exec(ctx, `UPDATE jobs SET state = 'dead', finished_at = now(), locked_by = NULL,
	locked_until = NULL, last_error = $3 WHERE id = $1 AND locked_by = $2`, id, q.owner, truncate(errMsg))
	case "retry", "returned":
		undo := 0
		if outcome == "returned" {
			undo = 1
		}
		_, err = q.pool.Exec(ctx, `UPDATE jobs SET state = 'queued', run_at = now() + make_interval(secs => $3),
	attempts = attempts - $4, locked_by = NULL, locked_until = NULL, last_error = $5
WHERE id = $1 AND locked_by = $2`, id, q.owner, delay.Seconds(), undo, truncate(errMsg))
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// A job with the same key was enqueued meanwhile and waits:
			// it does this job's work.
			_, err = q.pool.Exec(ctx, `DELETE FROM jobs WHERE id = $1 AND locked_by = $2`, id, q.owner)
		}
		if err == nil && delay == 0 {
			q.wakeType(c.job.Type)
		}
	}
	if err != nil {
		q.log.Warn("job queue: recording the outcome failed; the job runs again when its lease expires",
			"job_id", id, "type", c.job.Type, "outcome", outcome, "error", err)
	}
}

func truncate(s string) string {
	const maxErr = 2000
	if len(s) > maxErr {
		return s[:maxErr]
	}
	return s
}

// maintain prunes finished jobs past the dedup window and dead jobs past
// their retention. Every node does it; the deletes are idempotent.
func (q *Queue) maintain() {
	defer q.wg.Done()
	t := time.NewTicker(maintenanceEvery)
	defer t.Stop()
	for {
		select {
		case <-q.stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), queueTimeout)
		_, err := q.pool.Exec(ctx, `DELETE FROM jobs WHERE (state = 'done' AND finished_at < now() - make_interval(secs => $1))
	OR (state = 'dead' AND finished_at < now() - make_interval(secs => $2))`,
			q.opts.DedupWindow.Seconds(), q.opts.DeadRetention.Seconds())
		cancel()
		if err != nil {
			q.log.Debug("job queue: prune failed", "error", err)
		}
	}
}

// retryDelay is base, 2*base, 4*base, ... capped at 10 minutes.
func retryDelay(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt && d < maxRetryDelay; i++ {
		d *= 2
	}
	return min(d, maxRetryDelay)
}

func safeRun(ctx context.Context, h core.JobHandler, j core.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("job panicked: %v", r)
		}
	}()
	return h(ctx, j)
}

// JobCounts are the jobs of one type per state.
type JobCounts struct {
	Queued, Running, Dead int64
}

// Counts returns the jobs per type and state, for metrics and the System
// status screen.
func (q *Queue) Counts(ctx context.Context) (map[string]JobCounts, error) {
	rows, err := q.pool.Query(ctx, `SELECT type, state, count(*) FROM jobs WHERE state IN ('queued', 'running', 'dead') GROUP BY type, state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]JobCounts{}
	for rows.Next() {
		var (
			typ, state string
			n          int64
		)
		if err := rows.Scan(&typ, &state, &n); err != nil {
			return nil, err
		}
		c := out[typ]
		switch state {
		case "queued":
			c.Queued = n
		case "running":
			c.Running = n
		case "dead":
			c.Dead = n
		}
		out[typ] = c
	}
	return out, rows.Err()
}

// Close stops all workers on this node: it stops claiming jobs, cancels the
// contexts of the running handlers and waits up to ShutdownWait for them to
// return, so none runs on after the services it uses are closed. Jobs that
// were interrupted go back to the queue; queued jobs stay queued. A handler
// that ignores its context past ShutdownWait keeps its lease until it
// returns or the lease expires, then the job runs elsewhere.
func (q *Queue) Close() {
	q.mu.Lock()
	if q.closing {
		q.mu.Unlock()
		return
	}
	q.closing = true
	close(q.stop)
	q.mu.Unlock()
	q.cancelRun()

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(q.opts.ShutdownWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		// The waiter goroutine ends when the handlers do.
		q.log.Warn("job handlers still running after shutdown wait; their jobs run again elsewhere when their lease expires",
			"shutdown_wait", q.opts.ShutdownWait.String())
	}
}
