// Package tasks runs Hangar's background tasks: the rows of the tasks table
// (built-in maintenance, cleanup policies, tasks added by administrators)
// and their runs in task_runs.
//
// # Lifecycle of a run
//
// A run is created as "queued" (Run now, or the scheduler leader when a
// task's cron schedule is due) and a JobType job naming it is enqueued. Any
// node's job handler claims the run (queued -> running) and executes it in
// the background, so a long run is not bound to the job queue's ack timeout.
// The executing node writes a heartbeat with the current progress, message
// and new log lines every few seconds and learns from the same statement
// whether a stop was requested. The run ends as succeeded, failed or
// cancelled and a task.finished event carries its final state; progress is
// published as task.progress events (throttled).
//
// # Failures
//
// The partial unique index on task_runs allows one active (queued or
// running) run per task. A run whose heartbeat is older than StaleAfter is
// abandoned by its node (crash, network partition): the scheduler leader
// re-enqueues it and another node takes it over, resuming from the last
// checkpoint the executor saved (Exec.SaveState). Every claim increments
// task_runs.claims, and every write of an executing node is conditioned on
// the claim number it holds, so a node that lost a run cannot overwrite the
// new owner. After MaxRecoveries takeovers the run is failed. Queued runs
// whose job got lost are re-enqueued the same way. On shutdown a node hands
// its running runs back to the queue.
package tasks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/tasks/cron"
)

// JobType is the job that asks a node to execute a run.
const JobType = "task.run"

// Run statuses.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// Task types implemented in this repository. TypeCleanup tasks are managed by
// cleanup policies and cannot be created through Create.
const (
	TypeCleanup         = "cleanup"
	TypeBlobCompaction  = "blob_compaction"
	TypeDockerGC        = "docker_gc"
	TypeAuthPurge       = "auth_purge"
	TypeAuditPartitions = "audit_partitions"
)

// Triggers recorded in task_runs.triggered_by besides a principal name.
const TriggerSchedule = "schedule"

// Defaults for Options.
const (
	DefaultStaleAfter     = 2 * time.Minute
	DefaultHeartbeat      = 2 * time.Second
	DefaultProgressEvery  = time.Second
	DefaultMaxConcurrent  = 4
	DefaultMaxRecoveries  = 5
	DefaultLogLimitBytes  = 256 << 10
	defaultShutdownWait   = 15 * time.Second
	maxMessageLength      = 2000
	policyTaskPrefix      = "cleanup/"
	runCursorVersionField = "v1"
)

// Errors specific to tasks. They wrap core sentinels so core.HTTPStatus maps them.
var (
	// ErrActiveRun: the task already has a queued or running run.
	ErrActiveRun = fmt.Errorf("task already has an active run: %w", core.ErrAlreadyExists)
	// ErrNoActiveRun: Stop found nothing to stop.
	ErrNoActiveRun = fmt.Errorf("task has no active run: %w", core.ErrNotFound)
	// ErrManaged: the task is managed elsewhere (built in, or by a cleanup policy).
	ErrManaged = fmt.Errorf("task is managed by the server: %w", core.ErrAlreadyExists)
)

// SystemActor is the principal name of runs, and of what they do.
const SystemActor = "system"

// SystemPrincipal is the principal executors run as: the built-in Admin role,
// so the services they call authorize and audit as "system".
func SystemPrincipal() *core.Principal {
	return &core.Principal{Name: SystemActor, Kind: core.PrincipalServiceAccount, Source: "system", Roles: []string{"Admin"}}
}

// Task is one row of the tasks table (schema Task in api/openapi.yaml).
type Task struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Type         string          `json:"type"`
	Description  string          `json:"description"`
	Schedule     *string         `json:"schedule"`
	ScheduleText string          `json:"schedule_text"`
	Config       json.RawMessage `json:"config"`
	Enabled      bool            `json:"enabled"`
	Builtin      bool            `json:"builtin"`
	// PolicyID is set for the task of a cleanup policy.
	PolicyID  string     `json:"policy_id,omitempty"`
	NextRunAt *time.Time `json:"next_run_at"`
	LastRun   *Run       `json:"last_run"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Run is one execution of a task (schema TaskRun; Log and Result only in
// the run detail).
type Run struct {
	ID              string     `json:"id"`
	TaskID          string     `json:"task_id"`
	TaskName        string     `json:"task_name"`
	TaskType        string     `json:"task_type"`
	Status          string     `json:"status"`
	Progress        float64    `json:"progress"`
	Message         string     `json:"message"`
	TriggeredBy     string     `json:"triggered_by"`
	Node            string     `json:"node"`
	CancelRequested bool       `json:"cancel_requested"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	// Result is the executor's summary of a finished run (JSON object).
	Result json.RawMessage `json:"result,omitempty"`
	// ComponentsDeleted and BytesFreed summarize Result (or the running
	// totals of an active run) for the task types that report them.
	ComponentsDeleted *int64  `json:"components_deleted,omitempty"`
	BytesFreed        *int64  `json:"bytes_freed,omitempty"`
	Log               *string `json:"log,omitempty"`
}

// Active reports whether the run is queued or running.
func (r *Run) Active() bool { return r.Status == StatusQueued || r.Status == StatusRunning }

// Input creates or replaces a task (schema TaskInput).
type Input struct {
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Schedule    *string         `json:"schedule"`
	Config      json.RawMessage `json:"config"`
	// Enabled defaults to true.
	Enabled *bool `json:"enabled"`
}

// Executor performs the runs of one task type.
type Executor interface {
	// Validate checks a task's config; empty means the type's defaults.
	Validate(config json.RawMessage) error
	// Execute performs a run. It must return promptly once ctx is done
	// (stop requested, shutdown, lost ownership), with ctx's error. To be
	// resumable after a crash it saves checkpoints with Exec.SaveState and
	// reads the last one with Exec.LoadState. Its context carries
	// SystemPrincipal.
	Execute(ctx context.Context, x *Exec) error
}

// TypeOptions describe a registered task type.
type TypeOptions struct {
	// Creatable allows administrators to add tasks of this type.
	Creatable bool
}

type registration struct {
	exec Executor
	opts TypeOptions
}

// Options configure a Service. Every field except Pool and Jobs is optional.
type Options struct {
	Jobs   core.JobQueue
	Events core.EventBus
	Audit  core.AuditLog
	Logger *slog.Logger
	// Node names this node in task_runs.node (default: the host name).
	Node string
	// StaleAfter is how old a heartbeat may get before the run is taken over.
	StaleAfter time.Duration
	// Heartbeat is how often an executing node writes progress and checks
	// for a stop request.
	Heartbeat time.Duration
	// ProgressEvery throttles task.progress events per run.
	ProgressEvery time.Duration
	// MaxConcurrent runs execute on this node at once; further jobs are
	// retried later by the queue.
	MaxConcurrent int
	// MaxRecoveries takeovers before a run is failed.
	MaxRecoveries int
	// LogLimitBytes caps task_runs.log (the newest lines are kept).
	LogLimitBytes int
	// Now overrides the clock used for schedules (tests).
	Now func() time.Time
}

// Service manages tasks and executes runs. It is safe for concurrent use.
type Service struct {
	pool *pgxpool.Pool
	opts Options
	log  *slog.Logger

	mu     sync.Mutex
	types  map[string]registration
	closed bool
	wg     sync.WaitGroup
	sem    chan struct{}

	base     context.Context
	stopBase context.CancelCauseFunc
}

// New returns a service. Register executors, then call Handle.
func New(pool *pgxpool.Pool, opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Node == "" {
		opts.Node, _ = os.Hostname()
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = DefaultStaleAfter
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = DefaultHeartbeat
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = DefaultProgressEvery
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = DefaultMaxConcurrent
	}
	if opts.MaxRecoveries <= 0 {
		opts.MaxRecoveries = DefaultMaxRecoveries
	}
	if opts.LogLimitBytes <= 0 {
		opts.LogLimitBytes = DefaultLogLimitBytes
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	base, stop := context.WithCancelCause(context.Background())
	return &Service{
		pool: pool, opts: opts, log: opts.Logger,
		types: map[string]registration{},
		sem:   make(chan struct{}, opts.MaxConcurrent),
		base:  base, stopBase: stop,
	}
}

// Register installs the executor of a task type. Register every type before
// Handle; registering a type twice replaces it.
func (s *Service) Register(typ string, e Executor, o TypeOptions) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.types[typ] = registration{exec: e, opts: o}
}

// Types returns the registered task types, sorted.
func (s *Service) Types() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.types))
	for t := range s.types {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

func (s *Service) registration(typ string) (registration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.types[typ]
	return r, ok
}

// ---- schedules ----

// ValidateSchedule checks an optional cron expression (nil or empty = manual only).
func ValidateSchedule(schedule *string) error {
	if schedule == nil || strings.TrimSpace(*schedule) == "" {
		return nil
	}
	_, err := cron.Parse(*schedule)
	return err
}

// NextRun is the next activation of schedule after now, nil for manual tasks
// or invalid schedules.
func NextRun(schedule *string, now time.Time) *time.Time {
	if schedule == nil || strings.TrimSpace(*schedule) == "" {
		return nil
	}
	c, err := cron.Parse(*schedule)
	if err != nil {
		return nil
	}
	n := c.Next(now)
	if n.IsZero() {
		return nil
	}
	return &n
}

// ScheduleText renders a schedule for the UI.
func ScheduleText(schedule *string) string {
	if schedule == nil || strings.TrimSpace(*schedule) == "" {
		return "Manual only"
	}
	c, err := cron.Parse(*schedule)
	if err != nil {
		return "Invalid schedule " + *schedule
	}
	return c.Describe()
}

func normSchedule(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	v := strings.TrimSpace(*s)
	return &v
}

// ---- reads ----

const taskCols = `t.id::text, t.name, t.type, t.description, t.schedule, t.config, t.enabled, t.builtin,
	coalesce(t.policy_id::text, ''), t.next_run_at, t.created_at, t.updated_at`

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	var cfg []byte
	if err := row.Scan(&t.ID, &t.Name, &t.Type, &t.Description, &t.Schedule, &cfg, &t.Enabled, &t.Builtin,
		&t.PolicyID, &t.NextRunAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Config = json.RawMessage(cfg)
	if len(t.Config) == 0 {
		t.Config = json.RawMessage("{}")
	}
	t.ScheduleText = ScheduleText(t.Schedule)
	t.CreatedAt, t.UpdatedAt = t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	if t.NextRunAt != nil {
		v := t.NextRunAt.UTC()
		t.NextRunAt = &v
	}
	if !t.Enabled || t.Schedule == nil {
		t.NextRunAt = nil
	}
	return &t, nil
}

const runCols = `r.id::text, r.task_id::text, t.name, t.type, r.status, r.progress, r.message, r.triggered_by, r.node,
	r.cancel_requested, r.created_at, r.started_at, r.finished_at, r.result, r.state`

func scanRun(row pgx.Row, extra ...any) (*Run, error) {
	var r Run
	var result, state []byte
	var progress float32
	dest := append([]any{&r.ID, &r.TaskID, &r.TaskName, &r.TaskType, &r.Status, &progress, &r.Message, &r.TriggeredBy,
		&r.Node, &r.CancelRequested, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &result, &state}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	r.Progress = float64(progress)
	if len(result) > 0 {
		summarize(&r, result)
	} else if r.Active() {
		summarize(&r, state)
	}
	if len(result) > 0 {
		r.Result = json.RawMessage(result)
	}
	r.CreatedAt = r.CreatedAt.UTC()
	for _, p := range []**time.Time{&r.StartedAt, &r.FinishedAt} {
		if *p != nil {
			v := (*p).UTC()
			*p = &v
		}
	}
	return &r, nil
}

// List returns every task by name with its last run, and the active runs.
func (s *Service) List(ctx context.Context) ([]*Task, []*Run, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+taskCols+` FROM tasks t ORDER BY t.builtin DESC, t.name`)
	if err != nil {
		return nil, nil, fmt.Errorf("list tasks: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Task, error) { return scanTask(r) })
	if err != nil {
		return nil, nil, fmt.Errorf("list tasks: %w", err)
	}
	rows, err = s.pool.Query(ctx, `
SELECT DISTINCT ON (r.task_id) `+runCols+`
FROM task_runs r JOIN tasks t ON t.id = r.task_id
ORDER BY r.task_id, r.created_at DESC, r.id DESC`)
	if err != nil {
		return nil, nil, fmt.Errorf("list last runs: %w", err)
	}
	last, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Run, error) { return scanRun(r) })
	if err != nil {
		return nil, nil, fmt.Errorf("list last runs: %w", err)
	}
	byTask := make(map[string]*Run, len(last))
	for _, r := range last {
		byTask[r.TaskID] = r
	}
	for _, t := range list {
		t.LastRun = byTask[t.ID]
	}
	active, err := s.ActiveRuns(ctx)
	if err != nil {
		return nil, nil, err
	}
	return list, active, nil
}

// ActiveRuns returns the queued and running runs, oldest first.
func (s *Service) ActiveRuns(ctx context.Context) ([]*Run, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+runCols+` FROM task_runs r JOIN tasks t ON t.id = r.task_id
WHERE r.status IN ('queued', 'running') ORDER BY r.created_at, r.id`)
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Run, error) { return scanRun(r) })
	if err != nil {
		return nil, fmt.Errorf("list active runs: %w", err)
	}
	return out, nil
}

// Get returns a task by id or name, with its last run.
func (s *Service) Get(ctx context.Context, ref string) (*Task, error) {
	q := `SELECT ` + taskCols + ` FROM tasks t WHERE t.name = $1`
	if isUUID(ref) {
		q = `SELECT ` + taskCols + ` FROM tasks t WHERE t.id = $1::uuid`
	}
	t, err := scanTask(s.pool.QueryRow(ctx, q, ref))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("task %q: %w", ref, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get task %q: %w", ref, err)
	}
	t.LastRun, err = scanRunOrNil(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM task_runs r JOIN tasks t ON t.id = r.task_id
WHERE r.task_id = $1 ORDER BY r.created_at DESC, r.id DESC LIMIT 1`, t.ID))
	if err != nil {
		return nil, fmt.Errorf("get task %q: %w", ref, err)
	}
	return t, nil
}

func scanRunOrNil(row pgx.Row) (*Run, error) {
	r, err := scanRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

type runCursor struct {
	V  string    `json:"v"`
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

// Runs lists a task's runs, newest first.
func (s *Service) Runs(ctx context.Context, taskRef, cursor string, limit int) (core.Page[*Run], error) {
	t, err := s.Get(ctx, taskRef)
	if err != nil {
		return core.Page[*Run]{}, err
	}
	limit = core.ClampLimit(limit)
	args := []any{t.ID, limit + 1}
	where := ""
	if cursor != "" {
		var c runCursor
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err == nil {
			err = json.Unmarshal(b, &c)
		}
		if err != nil || c.V != runCursorVersionField || !isUUID(c.ID) {
			return core.Page[*Run]{}, fmt.Errorf("cursor %q: %w", cursor, core.ErrInvalid)
		}
		where = ` AND (r.created_at, r.id) < ($3, $4::uuid)`
		args = append(args, c.At, c.ID)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+runCols+` FROM task_runs r JOIN tasks t ON t.id = r.task_id
WHERE r.task_id = $1`+where+` ORDER BY r.created_at DESC, r.id DESC LIMIT $2`, args...)
	if err != nil {
		return core.Page[*Run]{}, fmt.Errorf("list runs: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Run, error) { return scanRun(r) })
	if err != nil {
		return core.Page[*Run]{}, fmt.Errorf("list runs: %w", err)
	}
	p := core.Page[*Run]{Items: items}
	if len(items) > limit {
		p.Items = items[:limit]
		last := p.Items[limit-1]
		b, _ := json.Marshal(runCursor{V: runCursorVersionField, At: last.CreatedAt, ID: last.ID})
		p.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return p, nil
}

// GetRun returns one run of a task with its log and result.
func (s *Service) GetRun(ctx context.Context, taskRef, runID string) (*Run, error) {
	t, err := s.Get(ctx, taskRef)
	if err != nil {
		return nil, err
	}
	if !isUUID(runID) {
		return nil, fmt.Errorf("run %q: %w", runID, core.ErrNotFound)
	}
	var log string
	r, err := scanRun(s.pool.QueryRow(ctx, `SELECT `+runCols+`, r.log FROM task_runs r JOIN tasks t ON t.id = r.task_id
WHERE r.id = $1 AND r.task_id = $2`, runID, t.ID), &log)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("run %q: %w", runID, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get run %q: %w", runID, err)
	}
	r.Log = &log
	return r, nil
}

// ---- writes ----

var taskNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

func (s *Service) validateInput(in *Input) (registration, error) {
	if !taskNameRE.MatchString(in.Name) {
		return registration{}, fmt.Errorf("task name %q must match %s: %w", in.Name, taskNameRE, core.ErrInvalid)
	}
	reg, ok := s.registration(in.Type)
	if !ok {
		return registration{}, fmt.Errorf("task type %q is not available on this server (available: %s): %w",
			in.Type, strings.Join(s.creatableTypes(), ", "), core.ErrInvalid)
	}
	in.Schedule = normSchedule(in.Schedule)
	if err := ValidateSchedule(in.Schedule); err != nil {
		return registration{}, err
	}
	if len(in.Config) == 0 || string(in.Config) == "null" {
		in.Config = json.RawMessage("{}")
	}
	var obj map[string]any
	if err := json.Unmarshal(in.Config, &obj); err != nil {
		return registration{}, fmt.Errorf("task config must be a JSON object: %w", core.ErrInvalid)
	}
	if err := reg.exec.Validate(in.Config); err != nil {
		return registration{}, err
	}
	if len(in.Description) > 1000 {
		return registration{}, fmt.Errorf("description longer than 1000 characters: %w", core.ErrInvalid)
	}
	return reg, nil
}

func (s *Service) creatableTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for t, r := range s.types {
		if r.opts.Creatable {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

func enabled(p *bool) bool { return p == nil || *p }

// Create adds a task of a creatable type. The caller authorizes; Create audits.
func (s *Service) Create(ctx context.Context, in Input) (*Task, error) {
	reg, err := s.validateInput(&in)
	if err != nil {
		return nil, err
	}
	if !reg.opts.Creatable {
		return nil, fmt.Errorf("tasks of type %q are managed by the server (cleanup tasks belong to cleanup policies): %w",
			in.Type, core.ErrInvalid)
	}
	var id string
	err = s.pool.QueryRow(ctx, `
INSERT INTO tasks (name, type, description, schedule, config, enabled, next_run_at)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
		in.Name, in.Type, in.Description, in.Schedule, []byte(in.Config), enabled(in.Enabled), NextRun(in.Schedule, s.opts.Now())).Scan(&id)
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("task %q: %w", in.Name, core.ErrAlreadyExists)
	}
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}
	s.audit(ctx, "task.create", "task:"+in.Name, map[string]any{"type": in.Type, "schedule": in.Schedule})
	return s.Get(ctx, id)
}

// Update replaces a task's description, schedule, config and enabled flag.
// Name and type cannot change. Tasks of cleanup policies are edited through
// their policy. The caller authorizes; Update audits.
func (s *Service) Update(ctx context.Context, ref string, in Input) (*Task, error) {
	cur, err := s.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if cur.PolicyID != "" {
		return nil, fmt.Errorf("task %q belongs to a cleanup policy; edit the policy instead: %w", cur.Name, ErrManaged)
	}
	if in.Name == "" {
		in.Name = cur.Name
	}
	if in.Type == "" {
		in.Type = cur.Type
	}
	if in.Name != cur.Name || in.Type != cur.Type {
		return nil, fmt.Errorf("task name and type cannot be changed: %w", core.ErrInvalid)
	}
	if _, err := s.validateInput(&in); err != nil {
		return nil, err
	}
	_, err = s.pool.Exec(ctx, `
UPDATE tasks SET description = $2, schedule = $3, config = $4, enabled = $5, next_run_at = $6, updated_at = now()
WHERE id = $1`, cur.ID, in.Description, in.Schedule, []byte(in.Config), enabled(in.Enabled), NextRun(in.Schedule, s.opts.Now()))
	if err != nil {
		return nil, fmt.Errorf("update task %q: %w", cur.Name, err)
	}
	s.audit(ctx, "task.update", "task:"+cur.Name, map[string]any{"schedule": in.Schedule, "enabled": enabled(in.Enabled)})
	return s.Get(ctx, cur.ID)
}

// Delete removes a task added by an administrator, with its runs. Built-in
// tasks and the tasks of cleanup policies cannot be deleted; a task with an
// active run must be stopped first.
func (s *Service) Delete(ctx context.Context, ref string) error {
	cur, err := s.Get(ctx, ref)
	if err != nil {
		return err
	}
	switch {
	case cur.Builtin:
		return fmt.Errorf("task %q is built in; disable it instead: %w", cur.Name, ErrManaged)
	case cur.PolicyID != "":
		return fmt.Errorf("task %q belongs to a cleanup policy; delete the policy instead: %w", cur.Name, ErrManaged)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM tasks WHERE id = $1
AND NOT EXISTS (SELECT 1 FROM task_runs WHERE task_id = $1 AND status IN ('queued', 'running'))`, cur.ID)
	if err != nil {
		return fmt.Errorf("delete task %q: %w", cur.Name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("task %q is running; stop it first: %w", cur.Name, ErrActiveRun)
	}
	s.audit(ctx, "task.delete", "task:"+cur.Name, nil)
	return nil
}

// PolicyTask describes the task of a cleanup policy (see SyncPolicyTask).
type PolicyTask struct {
	PolicyID    string
	PolicyName  string
	Description string
	Schedule    *string
	Enabled     bool
}

// PolicyTaskName is the task name of cleanup policy name.
func PolicyTaskName(policy string) string { return policyTaskPrefix + policy }

// SyncPolicyTask creates or updates the task of a cleanup policy inside the
// policy's transaction and returns its id. Deleting the policy deletes the
// task and its runs (foreign key cascade).
func SyncPolicyTask(ctx context.Context, tx pgx.Tx, p PolicyTask, now time.Time) (string, error) {
	sched := normSchedule(p.Schedule)
	if err := ValidateSchedule(sched); err != nil {
		return "", err
	}
	cfg, _ := json.Marshal(map[string]string{"policy_id": p.PolicyID})
	var id string
	err := tx.QueryRow(ctx, `
INSERT INTO tasks (name, type, description, schedule, config, enabled, policy_id, next_run_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (policy_id) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description,
	schedule = EXCLUDED.schedule, config = EXCLUDED.config, enabled = EXCLUDED.enabled,
	next_run_at = EXCLUDED.next_run_at, updated_at = now()
RETURNING id::text`,
		PolicyTaskName(p.PolicyName), TypeCleanup, p.Description, sched, cfg, p.Enabled, p.PolicyID, NextRun(sched, now)).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("sync task of cleanup policy %q: %w", p.PolicyName, err)
	}
	return id, nil
}

// Builtin is a task the server provides.
type Builtin struct {
	Name        string
	Type        string
	Description string
	// Schedule is the default cron schedule; administrators may change it.
	Schedule string
	// Config is the default config (JSON object); administrators may change it.
	Config any
}

// EnsureBuiltins creates the built-in tasks that do not exist yet. Existing
// rows keep the schedule, config and enabled flag an administrator chose;
// their type and description follow the server.
func (s *Service) EnsureBuiltins(ctx context.Context, list []Builtin) error {
	now := s.opts.Now()
	for _, b := range list {
		if _, ok := s.registration(b.Type); !ok {
			return fmt.Errorf("built-in task %q: type %q has no executor: %w", b.Name, b.Type, core.ErrInvalid)
		}
		sched := normSchedule(&b.Schedule)
		if err := ValidateSchedule(sched); err != nil {
			return fmt.Errorf("built-in task %q: %w", b.Name, err)
		}
		cfg := []byte("{}")
		if b.Config != nil {
			var err error
			if cfg, err = json.Marshal(b.Config); err != nil {
				return fmt.Errorf("built-in task %q config: %w", b.Name, err)
			}
		}
		_, err := s.pool.Exec(ctx, `
INSERT INTO tasks (name, type, description, schedule, config, builtin, next_run_at)
VALUES ($1, $2, $3, $4, $5, true, $6)
ON CONFLICT (name) DO UPDATE SET type = EXCLUDED.type, description = EXCLUDED.description, builtin = true
WHERE tasks.policy_id IS NULL`, b.Name, b.Type, b.Description, sched, cfg, NextRun(sched, now))
		if err != nil {
			return fmt.Errorf("ensure built-in task %q: %w", b.Name, err)
		}
	}
	return nil
}

// Func is an Executor for task types without configuration: the config must
// be an empty object.
type Func func(ctx context.Context, x *Exec) error

// Validate implements Executor.
func (Func) Validate(cfg json.RawMessage) error {
	var m map[string]any
	if err := json.Unmarshal(cfg, &m); err != nil || len(m) != 0 {
		return fmt.Errorf("this task type takes no configuration: %w", core.ErrInvalid)
	}
	return nil
}

// Execute implements Executor.
func (f Func) Execute(ctx context.Context, x *Exec) error { return f(ctx, x) }

// ---- helpers ----

func (s *Service) audit(ctx context.Context, action, target string, details map[string]any) {
	if s.opts.Audit == nil {
		return
	}
	if err := s.opts.Audit.Record(ctx, core.AuditEvent{Action: action, Target: target, Details: details}); err != nil {
		s.log.WarnContext(ctx, "audit record failed", "action", action, "target", target, "error", err)
	}
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // do not split a UTF-8 sequence
		cut--
	}
	return s[:cut] + "…"
}
