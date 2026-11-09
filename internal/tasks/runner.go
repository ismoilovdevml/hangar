package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Causes of a run context's cancellation.
var (
	errStopRequested = errors.New("stop requested")
	errShutdown      = errors.New("node shutting down")
	errLostRun       = errors.New("run was taken over by another node")
	errBusy          = errors.New("node is executing its maximum number of runs")
)

// ErrLostRun is returned by Exec.SaveState when another node took the run over.
var ErrLostRun = errLostRun

type jobPayload struct {
	RunID string `json:"run_id"`
}

// Handle registers the run job handler on the job queue.
func (s *Service) Handle() error {
	if s.opts.Jobs == nil {
		return fmt.Errorf("tasks: no job queue: %w", core.ErrInvalid)
	}
	return s.opts.Jobs.Handle(JobType, s.HandleJob)
}

// HandleJob is the JobType handler: it claims the run and executes it in the
// background. It returns an error (so the queue retries) only when this node
// cannot take the run now.
func (s *Service) HandleJob(ctx context.Context, j core.Job) error {
	var p jobPayload
	if err := json.Unmarshal(j.Payload, &p); err != nil || !isUUID(p.RunID) {
		// Retrying cannot fix the payload: acknowledge and drop it.
		s.log.Error("tasks: dropping malformed run job", "job_id", j.ID, "payload", string(j.Payload), "error", err)
		return nil
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return errBusy
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.sem
		return errShutdown
	}
	s.wg.Add(1)
	s.mu.Unlock()
	release := func() { <-s.sem; s.wg.Done() }

	c, err := s.claim(ctx, p.RunID)
	if err != nil || c == nil {
		release()
		return err
	}
	go func() {
		defer release()
		s.execute(c)
	}()
	return nil
}

type claimed struct {
	run    *Run
	task   *Task
	claims int
	state  json.RawMessage
}

// claim moves a queued run (or a running one whose node went silent) to
// running on this node. It returns nil when the run is not claimable:
// finished, cancelled, or alive on another node.
func (s *Service) claim(ctx context.Context, runID string) (*claimed, error) {
	var (
		c        claimed
		state    []byte
		recovers int
		took     bool
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var prev string
		err := tx.QueryRow(ctx, `SELECT status FROM task_runs WHERE id = $1 FOR UPDATE`, runID).Scan(&prev)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
UPDATE task_runs SET status = 'running', started_at = coalesce(started_at, now()), heartbeat_at = now(),
	node = $2, claims = claims + 1,
	recoveries = recoveries + CASE WHEN status = 'running' THEN 1 ELSE 0 END
WHERE id = $1 AND (status = 'queued'
	OR (status = 'running' AND heartbeat_at < now() - make_interval(secs => $3) AND recoveries < $4))
RETURNING claims, recoveries, state`, runID, s.opts.Node, s.opts.StaleAfter.Seconds(), s.opts.MaxRecoveries).
			Scan(&c.claims, &recovers, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		took = true
		if prev == StatusRunning {
			s.log.Warn("tasks: taking over a run whose node stopped responding", "run_id", runID, "recoveries", recovers)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim run %s: %w", runID, err)
	}
	if !took {
		return nil, nil
	}
	run, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM task_runs r JOIN tasks t ON t.id = r.task_id WHERE r.id = $1`, runID))
	if err != nil {
		return nil, fmt.Errorf("load claimed run %s: %w", runID, err)
	}
	task, err := s.get(ctx, run.TaskID)
	if err != nil {
		return nil, fmt.Errorf("load task of run %s: %w", runID, err)
	}
	c.run, c.task = run, task
	if len(state) > 0 && string(state) != "null" {
		c.state = state
	}
	return &c, nil
}

// Exec is what an executor gets for one run: the task, the run, progress and
// log reporting, and checkpoints. Its methods are safe for concurrent use.
type Exec struct {
	Task *Task
	Run  *Run

	s      *Service
	claims int
	state  json.RawMessage

	mu        sync.Mutex
	progress  float64
	message   string
	dirty     bool
	logBuf    strings.Builder
	result    json.RawMessage
	lastEvent time.Time
	lastSent  float64
}

// Progress records the fraction done (0..1) and a one-line status message.
// It is written with the next heartbeat and published as task.progress.
func (x *Exec) Progress(fraction float64, message string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.progress = min(max(fraction, 0), 1)
	if message != "" {
		x.message = truncate(message, maxMessageLength)
	}
	x.dirty = true
}

// Logf appends an info line to the run log.
func (x *Exec) Logf(format string, args ...any) { x.logLine(LevelInfo, fmt.Sprintf(format, args...)) }

// Warnf appends a warning line to the run log (a step failed, the run goes on).
func (x *Exec) Warnf(format string, args ...any) { x.logLine(LevelWarn, fmt.Sprintf(format, args...)) }

func (x *Exec) logLine(level, line string) {
	x.mu.Lock()
	x.logBuf.WriteString(formatLogLine(time.Now(), level, line))
	x.dirty = true
	x.mu.Unlock()
	x.s.log.Debug("task log", "task", x.Task.Name, "run_id", x.Run.ID, "level", level, "line", line)
}

// SetResult sets the summary stored with the finished run (a JSON object).
func (x *Exec) SetResult(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		x.s.log.Warn("tasks: result not encodable", "run_id", x.Run.ID, "error", err)
		return
	}
	x.mu.Lock()
	x.result = b
	x.mu.Unlock()
}

// LoadState decodes the checkpoint saved by an earlier claim of this run
// into v and reports whether there was one.
func (x *Exec) LoadState(v any) (bool, error) {
	if len(x.state) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(x.state, v); err != nil {
		return false, fmt.Errorf("decode checkpoint: %w", err)
	}
	return true, nil
}

// SaveState stores a checkpoint, together with the pending progress and
// log. It returns ErrLostRun if another node owns the run now; the executor
// must then stop.
func (x *Exec) SaveState(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode checkpoint: %w", err)
	}
	if _, err := x.flush(ctx, b); err != nil {
		return err
	}
	x.mu.Lock()
	x.state = b
	x.mu.Unlock()
	return nil
}

// Resumed reports whether this claim continues an earlier one (checkpoint present).
func (x *Exec) Resumed() bool { return len(x.state) > 0 }

// pending takes the unwritten progress and log.
func (x *Exec) pending() (progress float64, message, log string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	log = x.logBuf.String()
	x.logBuf.Reset()
	x.dirty = false
	return x.progress, x.message, log
}

// unflush puts log lines back after a failed write.
func (x *Exec) unflush(log string) {
	if log == "" {
		return
	}
	x.mu.Lock()
	rest := x.logBuf.String()
	x.logBuf.Reset()
	x.logBuf.WriteString(log)
	x.logBuf.WriteString(rest)
	x.dirty = true
	x.mu.Unlock()
}

// flush writes the heartbeat, progress, message, new log lines and (when
// state is not nil) the checkpoint. It reports whether a stop was requested.
func (x *Exec) flush(ctx context.Context, state []byte) (stop bool, err error) {
	progress, message, log := x.pending()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err = x.s.pool.QueryRow(wctx, `
UPDATE task_runs SET heartbeat_at = now(), progress = $3, message = $4,
	log = right(log || $5, $6), state = coalesce($7, state)
WHERE id = $1 AND claims = $2 AND status = 'running'
RETURNING cancel_requested`, x.Run.ID, x.claims, progress, message, log, x.s.opts.LogLimitBytes, state).Scan(&stop)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrLostRun
	}
	if err != nil {
		x.unflush(log)
		return false, fmt.Errorf("write run heartbeat: %w", err)
	}
	return stop, nil
}

// execute runs a claimed run to its end on this node.
func (s *Service) execute(c *claimed) {
	ctx, cancel := context.WithCancelCause(s.base)
	defer cancel(nil)
	ctx = core.WithPrincipal(ctx, SystemPrincipal())
	x := &Exec{Task: c.task, Run: c.run, s: s, claims: c.claims, state: c.state}
	if c.run.Message != "" && c.state != nil {
		x.message = c.run.Message
	}
	x.progress = c.run.Progress
	log := s.log.With("task", c.task.Name, "type", c.task.Type, "run_id", c.run.ID)
	log.Info("task run started", "triggered_by", c.run.TriggeredBy, "resumed", x.Resumed())
	if x.Resumed() {
		x.Logf("Resuming on %s from the last checkpoint", s.opts.Node)
	} else {
		x.Logf("Started on %s (triggered by %s)", s.opts.Node, c.run.TriggeredBy)
	}
	s.publishProgress(ctx, x, true)

	stopBeat := make(chan struct{})
	beatDone := make(chan struct{})
	go s.heartbeat(ctx, x, cancel, stopBeat, beatDone)

	var err error
	reg, ok := s.registration(c.task.Type)
	if !ok {
		err = fmt.Errorf("task type %q cannot run on this server: %w", c.task.Type, core.ErrNotImplemented)
	} else {
		err = safeExecute(ctx, reg.exec, x)
	}
	cause := context.Cause(ctx)
	close(stopBeat)
	<-beatDone

	switch {
	case errors.Is(cause, errLostRun) || errors.Is(err, ErrLostRun):
		log.Warn("task run lost to another node; stopping here")
	case err == nil:
		s.finish(x, StatusSucceeded, "", log)
	case errors.Is(cause, errStopRequested):
		x.Logf("Stopped on request")
		s.finish(x, StatusCancelled, "Stopped on request", log)
	case errors.Is(cause, errShutdown):
		s.release(x, log)
	default:
		x.logLine(LevelError, "Failed: "+err.Error())
		s.finish(x, StatusFailed, err.Error(), log)
	}
}

func safeExecute(ctx context.Context, e Executor, x *Exec) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("task panicked: %v", r)
		}
	}()
	return e.Execute(ctx, x)
}

func (s *Service) heartbeat(ctx context.Context, x *Exec, cancel context.CancelCauseFunc, stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(s.opts.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		stopReq, err := x.flush(ctx, nil)
		switch {
		case errors.Is(err, ErrLostRun):
			cancel(errLostRun)
			return
		case err != nil:
			s.log.Warn("tasks: heartbeat failed", "run_id", x.Run.ID, "error", err)
			continue
		case stopReq:
			cancel(errStopRequested)
		}
		s.publishProgress(ctx, x, false)
	}
}

// finish records the end of a run and publishes task.finished.
func (s *Service) finish(x *Exec, status, message string, log *slog.Logger) {
	progress, last, lines := x.pending()
	if message == "" {
		message = last
	}
	if message == "" {
		message = "Completed"
	}
	if status == StatusSucceeded {
		progress = 1
	}
	x.mu.Lock()
	result := x.result
	x.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	run, err := scanRun(s.pool.QueryRow(ctx, `
WITH u AS (
	UPDATE task_runs SET status = $3, progress = $4, message = $5, result = $6,
		log = right(log || $7, $8), finished_at = now(), heartbeat_at = now()
	WHERE id = $1 AND claims = $2 AND status = 'running'
	RETURNING *
)
SELECT `+strings.ReplaceAll(runCols, "r.", "u.")+` FROM u JOIN tasks t ON t.id = u.task_id`,
		x.Run.ID, x.claims, status, progress, truncate(message, maxMessageLength), result, lines, s.opts.LogLimitBytes))
	if errors.Is(err, pgx.ErrNoRows) {
		log.Warn("task run finished after it was taken over; result dropped", "status", status)
		return
	}
	if err != nil {
		// The heartbeat goes stale and another node retries the run.
		log.Warn("task run: recording the result failed", "status", status, "error", err)
		return
	}
	log.Info("task run finished", "status", status, "message", run.Message)
	s.publishFinished(ctx, run)
}

// release hands a run back to the queue (shutdown), to be resumed by any node.
func (s *Service) release(x *Exec, log *slog.Logger) {
	progress, message, lines := x.pending()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tag, err := s.pool.Exec(ctx, `
UPDATE task_runs SET status = 'queued', heartbeat_at = now(), progress = $3, message = $4,
	log = right(log || $5 || $6, $7)
WHERE id = $1 AND claims = $2 AND status = 'running'`, x.Run.ID, x.claims, progress, message, lines,
		formatLogLine(time.Now(), LevelWarn, "Interrupted: "+s.opts.Node+" is shutting down; the run will resume"),
		s.opts.LogLimitBytes)
	if err != nil || tag.RowsAffected() == 0 {
		log.Warn("task run: hand-back on shutdown failed; it is recovered when its heartbeat goes stale", "error", err)
		return
	}
	if err := s.enqueue(ctx, x.Run.ID, "release-"+strconv.Itoa(x.claims)); err != nil {
		log.Warn("task run: re-enqueue on shutdown failed; it is recovered by the scheduler", "error", err)
		return
	}
	log.Info("task run handed back to the queue")
}

func (s *Service) enqueue(ctx context.Context, runID, key string) error {
	b, _ := json.Marshal(jobPayload{RunID: runID})
	_, err := s.opts.Jobs.Enqueue(ctx, core.Job{Type: JobType, Key: runID + ":" + key, Payload: b})
	return err
}

// Close stops executing on this node: running runs are handed back to the
// queue (other nodes, or this one after a restart, resume them). It waits
// until ctx ends at most.
func (s *Service) Close(ctx context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	s.stopBase(errShutdown)
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultShutdownWait)
		defer cancel()
	}
	select {
	case <-done:
	case <-ctx.Done():
		s.log.Warn("tasks: shutdown did not wait for every run to hand back")
	}
}

// ---- triggering and stopping ----

// Trigger queues a run of a task. triggeredBy is a principal name or
// TriggerSchedule. It fails with ErrActiveRun when a run is active. Manual
// triggers are audited (the caller authorizes).
func (s *Service) Trigger(ctx context.Context, ref, triggeredBy string) (*Run, error) {
	t, err := s.get(ctx, ref)
	if err != nil {
		return nil, err
	}
	reg, ok := s.registration(t.Type)
	if !ok {
		return nil, fmt.Errorf("task type %q cannot run on this server: %w", t.Type, core.ErrNotImplemented)
	}
	if h, ok := reg.exec.(ConfigHooks); ok && triggeredBy != TriggerSchedule {
		if err := h.AuthorizeRun(ctx, t.Config); err != nil {
			return nil, err
		}
	}
	var runID string
	err = s.pool.QueryRow(ctx, `INSERT INTO task_runs (task_id, status, triggered_by, message, heartbeat_at)
VALUES ($1, 'queued', $2, 'Queued', now()) RETURNING id::text`, t.ID, triggeredBy).Scan(&runID)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("task %q: %w", t.Name, ErrActiveRun)
	}
	if err != nil {
		return nil, fmt.Errorf("queue run of task %q: %w", t.Name, err)
	}
	if err := s.enqueue(ctx, runID, "start"); err != nil {
		_, _ = s.pool.Exec(context.WithoutCancel(ctx), `UPDATE task_runs SET status = 'failed', finished_at = now(),
message = $2 WHERE id = $1 AND status = 'queued'`, runID, truncate("Could not queue the run: "+err.Error(), maxMessageLength))
		return nil, fmt.Errorf("queue run of task %q: %w", t.Name, err)
	}
	if triggeredBy != TriggerSchedule {
		s.audit(ctx, "task.run", "task:"+t.Name, map[string]any{"run_id": runID})
	}
	run, err := s.run(ctx, runID)
	if err != nil {
		return nil, err
	}
	s.publish(ctx, core.EventTaskProgress, run.ID, progressData(run))
	return run, nil
}

func (s *Service) run(ctx context.Context, id string) (*Run, error) {
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM task_runs r JOIN tasks t ON t.id = r.task_id WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("run %s: %w", id, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", id, err)
	}
	return r, nil
}

// Stop cancels the active run of a task: a queued run is cancelled at once,
// a running one is asked to stop and ends within a heartbeat or two. It
// fails with ErrNoActiveRun when nothing is active. Audited.
func (s *Service) Stop(ctx context.Context, ref string) (*Run, error) {
	t, err := s.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	var id, status string
	err = s.pool.QueryRow(ctx, `
UPDATE task_runs SET cancel_requested = true,
	status = CASE WHEN status = 'queued' THEN 'cancelled' ELSE status END,
	finished_at = CASE WHEN status = 'queued' THEN now() ELSE finished_at END,
	message = CASE WHEN status = 'queued' THEN 'Cancelled before it started' ELSE 'Stopping' END
WHERE task_id = $1 AND status IN ('queued', 'running')
RETURNING id::text, status`, t.ID).Scan(&id, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("task %q: %w", t.Name, ErrNoActiveRun)
	}
	if err != nil {
		return nil, fmt.Errorf("stop task %q: %w", t.Name, err)
	}
	s.audit(ctx, "task.stop", "task:"+t.Name, map[string]any{"run_id": id})
	run, err := s.run(ctx, id)
	if err != nil {
		return nil, err
	}
	if status == StatusCancelled {
		s.publishFinished(ctx, run)
	}
	return run, nil
}

// ---- events ----

func progressData(r *Run) map[string]any {
	return map[string]any{"task_id": r.TaskID, "run_id": r.ID, "task_name": r.TaskName,
		"status": r.Status, "progress": r.Progress, "message": r.Message}
}

func (s *Service) publishProgress(ctx context.Context, x *Exec, force bool) {
	x.mu.Lock()
	now := time.Now()
	if !force && (x.progress == x.lastSent || now.Sub(x.lastEvent) < s.opts.ProgressEvery) {
		x.mu.Unlock()
		return
	}
	x.lastEvent, x.lastSent = now, x.progress
	data := map[string]any{"task_id": x.Run.TaskID, "run_id": x.Run.ID, "task_name": x.Task.Name,
		"status": StatusRunning, "progress": x.progress, "message": x.message}
	x.mu.Unlock()
	s.publish(ctx, core.EventTaskProgress, x.Run.ID, data)
}

func (s *Service) publishFinished(ctx context.Context, r *Run) {
	s.publish(ctx, core.EventTaskFinished, r.ID, r)
}

func (s *Service) publish(ctx context.Context, typ, subject string, data any) {
	if s.opts.Events == nil {
		return
	}
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	e := core.Event{Type: typ, At: time.Now().UTC(), Actor: core.PrincipalFrom(ctx).Name, Subject: subject, Data: b}
	if err := s.opts.Events.Publish(pctx, e); err != nil {
		s.log.Warn("tasks: publish event failed", "type", typ, "error", err)
	}
}
