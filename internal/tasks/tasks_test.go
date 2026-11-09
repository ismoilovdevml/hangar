package tasks_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/core/coretest"
	"github.com/ismoilovdevml/hangar/internal/service/repository/pgtest"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// fakeExec is a configurable executor.
type fakeExec struct {
	validate func(json.RawMessage) error
	run      func(ctx context.Context, x *tasks.Exec) error
}

func (f fakeExec) Validate(c json.RawMessage) error {
	if f.validate != nil {
		return f.validate(c)
	}
	return nil
}

func (f fakeExec) Execute(ctx context.Context, x *tasks.Exec) error { return f.run(ctx, x) }

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	jobs   *coretest.JobQueue
	events *coretest.EventBus
	audit  *coretest.AuditLog
	seen   int // jobs already dispatched
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{t: t, pool: pgtest.Pool(t), jobs: coretest.NewJobQueue(), events: coretest.NewEventBus(), audit: coretest.NewAuditLog()}
}

func (e *env) service(node string, opts ...func(*tasks.Options)) *tasks.Service {
	o := tasks.Options{Jobs: e.jobs, Events: e.events, Audit: e.audit, Node: node,
		Heartbeat: 20 * time.Millisecond, ProgressEvery: time.Millisecond}
	for _, f := range opts {
		f(&o)
	}
	s := tasks.New(e.pool, o)
	e.t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

// dispatch hands every job enqueued since the last call to svc.
func (e *env) dispatch(svc *tasks.Service) {
	e.t.Helper()
	jobs := e.jobs.Jobs()
	for _, j := range jobs[e.seen:] {
		if j.Type != tasks.JobType {
			continue
		}
		if err := svc.HandleJob(context.Background(), j); err != nil {
			e.t.Fatalf("handle job: %v", err)
		}
	}
	e.seen = len(jobs)
}

func (e *env) waitRun(svc *tasks.Service, task, runID string, cond func(*tasks.Run) bool) *tasks.Run {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := svc.GetRun(context.Background(), task, runID)
		if err != nil {
			e.t.Fatal(err)
		}
		if cond(r) {
			return r
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("run %s stuck: %+v", runID, r)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func finished(r *tasks.Run) bool { return !r.Active() }

func asAdmin() context.Context { return coretest.AsUser(context.Background(), "admin") }

func ptr[T any](v T) *T { return &v }

func TestCreateUpdateDeleteAndBuiltins(t *testing.T) {
	e := newEnv(t)
	svc := e.service("n1")
	ctx := asAdmin()
	svc.Register("noop", fakeExec{
		validate: func(c json.RawMessage) error {
			if strings.Contains(string(c), "bad") {
				return core.ErrInvalid
			}
			return nil
		},
		run: func(context.Context, *tasks.Exec) error { return nil },
	}, tasks.TypeOptions{Creatable: true})
	svc.Register(tasks.TypeCleanup, fakeExec{run: func(context.Context, *tasks.Exec) error { return nil }}, tasks.TypeOptions{})

	if err := svc.EnsureBuiltins(ctx, []tasks.Builtin{{Name: "house-keeping", Type: "noop", Schedule: "0 3 * * *", Description: "v1"}}); err != nil {
		t.Fatal(err)
	}
	b, err := svc.Get(ctx, "house-keeping")
	if err != nil {
		t.Fatal(err)
	}
	if !b.Builtin || b.ScheduleText != "Every day at 03:00 UTC" || b.NextRunAt == nil || string(b.Config) != "{}" {
		t.Fatalf("builtin = %+v", b)
	}
	// An administrator's schedule survives a restart; description follows the server.
	if _, err := svc.Update(ctx, b.ID, tasks.Input{Schedule: ptr("0 4 * * *"), Enabled: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsureBuiltins(ctx, []tasks.Builtin{{Name: "house-keeping", Type: "noop", Schedule: "0 3 * * *", Description: "v2"}}); err != nil {
		t.Fatal(err)
	}
	b, _ = svc.Get(ctx, "house-keeping")
	if *b.Schedule != "0 4 * * *" || b.Enabled || b.Description != "v2" || b.NextRunAt != nil {
		t.Fatalf("builtin after restart = %+v", b)
	}
	if err := svc.Delete(ctx, b.ID); !errors.Is(err, tasks.ErrManaged) {
		t.Fatalf("delete builtin = %v", err)
	}

	bad := []tasks.Input{
		{Name: "Bad Name", Type: "noop"},
		{Name: "x1", Type: "unknown"},
		{Name: "x2", Type: "noop", Schedule: ptr("61 * * * *")},
		{Name: "x3", Type: "noop", Config: json.RawMessage(`{"bad":1}`)},
		{Name: "x4", Type: "noop", Config: json.RawMessage(`[1]`)},
		{Name: "x5", Type: tasks.TypeCleanup},
	}
	for _, in := range bad {
		if _, err := svc.Create(ctx, in); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("Create(%+v) = %v, want ErrInvalid", in, err)
		}
	}
	created, err := svc.Create(ctx, tasks.Input{Name: "nightly", Type: "noop", Schedule: ptr(" @daily "), Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if *created.Schedule != "@daily" || !created.Enabled || created.Builtin {
		t.Fatalf("created = %+v", created)
	}
	if _, err := svc.Create(ctx, tasks.Input{Name: "nightly", Type: "noop"}); !errors.Is(err, core.ErrAlreadyExists) {
		t.Fatalf("duplicate = %v", err)
	}
	if _, err := svc.Update(ctx, "nightly", tasks.Input{Name: "renamed", Type: "noop"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("rename = %v", err)
	}
	up, err := svc.Update(ctx, "nightly", tasks.Input{Type: "noop", Schedule: nil})
	if err != nil || up.Schedule != nil || up.ScheduleText != "Manual only" || up.NextRunAt != nil {
		t.Fatalf("update = %+v, %v", up, err)
	}
	if err := svc.Delete(ctx, "nightly"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, "nightly"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("get deleted = %v", err)
	}
	var actions []string
	for _, ev := range e.audit.Events() {
		actions = append(actions, ev.Action+"@"+ev.Actor)
	}
	if got := strings.Join(actions, ","); got != "task.update@admin,task.create@admin,task.update@admin,task.delete@admin" {
		t.Errorf("audit = %s", got)
	}
}

func TestRunLifecycle(t *testing.T) {
	e := newEnv(t)
	svc := e.service("node-a")
	ctx := asAdmin()
	svc.Register("work", fakeExec{run: func(ctx context.Context, x *tasks.Exec) error {
		if core.PrincipalFrom(ctx).Name != tasks.SystemActor {
			return errors.New("not running as system")
		}
		x.Logf("step %d", 1)
		x.Progress(0.5, "halfway")
		time.Sleep(60 * time.Millisecond) // a heartbeat or two
		x.SetResult(map[string]int{"items": 3})
		x.Progress(1, "Processed 3 items")
		return nil
	}}, tasks.TypeOptions{Creatable: true})
	svc.Register("broken", fakeExec{run: func(context.Context, *tasks.Exec) error { return errors.New("disk on fire") }},
		tasks.TypeOptions{Creatable: true})
	if _, err := svc.Create(ctx, tasks.Input{Name: "work", Type: "work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, tasks.Input{Name: "broken", Type: "broken"}); err != nil {
		t.Fatal(err)
	}

	run, err := svc.Trigger(ctx, "work", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != tasks.StatusQueued || run.TaskName != "work" || run.TriggeredBy != "admin" {
		t.Fatalf("queued run = %+v", run)
	}
	if _, err := svc.Trigger(ctx, "work", "admin"); !errors.Is(err, tasks.ErrActiveRun) {
		t.Fatalf("second trigger = %v", err)
	}
	e.dispatch(svc)
	done := e.waitRun(svc, "work", run.ID, finished)
	if done.Status != tasks.StatusSucceeded || done.Progress != 1 || done.Message != "Processed 3 items" || done.Node != "node-a" ||
		string(done.Result) != `{"items": 3}` || done.StartedAt == nil || done.FinishedAt == nil {
		t.Fatalf("finished run = %+v result=%s", done, done.Result)
	}
	if !strings.Contains(*done.Log, "step 1") || !strings.Contains(*done.Log, "Started on node-a") {
		t.Errorf("log = %q", *done.Log)
	}
	lines, err := svc.RunLog(ctx, "work", run.ID, 0, 0)
	if err != nil || len(lines.Items) != 2 || lines.Truncated || lines.Items[1].Seq != 2 ||
		lines.Items[1].Message != "step 1" || lines.Items[1].Level != tasks.LevelInfo || lines.Items[1].At.IsZero() {
		t.Fatalf("run log = %+v, %v", lines, err)
	}
	if tail, _ := svc.RunLog(ctx, "work", run.ID, 1, 10); len(tail.Items) != 1 || tail.Items[0].Seq != 2 {
		t.Fatalf("log after 1 = %+v", tail)
	}

	var progress, fin int
	for _, ev := range e.events.Published() {
		switch ev.Type {
		case core.EventTaskProgress:
			progress++
			var d map[string]any
			_ = json.Unmarshal(ev.Data, &d)
			if d["task_id"] != run.TaskID || d["run_id"] != run.ID {
				t.Errorf("progress event = %s", ev.Data)
			}
		case core.EventTaskFinished:
			fin++
			var r tasks.Run
			_ = json.Unmarshal(ev.Data, &r)
			if r.ID != run.ID || r.Status != tasks.StatusSucceeded {
				t.Errorf("finished event = %s", ev.Data)
			}
		}
	}
	if progress < 2 || fin != 1 {
		t.Errorf("events: %d progress, %d finished", progress, fin)
	}

	// A failing executor fails the run with its error; the task can run again.
	failed, err := svc.Trigger(ctx, "broken", "admin")
	if err != nil {
		t.Fatal(err)
	}
	e.dispatch(svc)
	failed = e.waitRun(svc, "broken", failed.ID, finished)
	if failed.Status != tasks.StatusFailed || failed.Message != "disk on fire" {
		t.Fatalf("failed run = %+v", failed)
	}
	if fl, _ := svc.RunLog(ctx, "broken", failed.ID, 0, 0); len(fl.Items) == 0 ||
		fl.Items[len(fl.Items)-1].Level != tasks.LevelError {
		t.Fatalf("failure not logged as error: %+v", fl)
	}
	page, err := svc.Runs(ctx, "work", "", 1)
	if err != nil || len(page.Items) != 1 || page.NextCursor != "" || page.Items[0].ID != run.ID {
		t.Fatalf("runs = %+v, %v", page, err)
	}
	list, active, err := svc.List(ctx)
	if err != nil || len(list) != 2 || len(active) != 0 || list[1].LastRun == nil || list[1].LastRun.ID != run.ID {
		t.Fatalf("list = %+v active=%+v err=%v", list, active, err)
	}
}

func TestStop(t *testing.T) {
	e := newEnv(t)
	svc := e.service("n1")
	ctx := asAdmin()
	started := make(chan struct{})
	svc.Register("long", fakeExec{run: func(ctx context.Context, x *tasks.Exec) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}, tasks.TypeOptions{Creatable: true})
	if _, err := svc.Create(ctx, tasks.Input{Name: "long", Type: "long"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Stop(ctx, "long"); !errors.Is(err, tasks.ErrNoActiveRun) {
		t.Fatalf("stop idle = %v", err)
	}

	// A queued run is cancelled at once and never executes.
	q, err := svc.Trigger(ctx, "long", "admin")
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := svc.Stop(ctx, "long")
	if err != nil || stopped.ID != q.ID || stopped.Status != tasks.StatusCancelled {
		t.Fatalf("stop queued = %+v, %v", stopped, err)
	}
	e.dispatch(svc) // the job arrives late: nothing to claim

	// A running run stops within a heartbeat.
	r, err := svc.Trigger(ctx, "long", "admin")
	if err != nil {
		t.Fatal(err)
	}
	e.dispatch(svc)
	<-started
	at := time.Now()
	if _, err := svc.Stop(ctx, "long"); err != nil {
		t.Fatal(err)
	}
	done := e.waitRun(svc, "long", r.ID, finished)
	if done.Status != tasks.StatusCancelled || done.Message != "Stopped on request" || !done.CancelRequested {
		t.Fatalf("stopped run = %+v", done)
	}
	if d := time.Since(at); d > 2*time.Second {
		t.Errorf("stop took %v", d)
	}
}

// A node that dies mid-run loses the run to another node, which resumes it
// from the checkpoint; the dead node can no longer write.
func TestTakeoverResumesFromCheckpoint(t *testing.T) {
	e := newEnv(t)
	ctx := asAdmin()
	var calls atomic.Int32
	blockA := make(chan struct{})
	staleWrite := make(chan error, 1)
	var stateA *tasks.Exec
	var mu sync.Mutex
	exec := fakeExec{run: func(ctx context.Context, x *tasks.Exec) error {
		var st struct{ Done int }
		resumed, err := x.LoadState(&st)
		if err != nil {
			return err
		}
		if calls.Add(1) == 1 {
			if resumed {
				return errors.New("first claim saw a checkpoint")
			}
			if err := x.SaveState(ctx, struct{ Done int }{Done: 7}); err != nil {
				return err
			}
			mu.Lock()
			stateA = x
			mu.Unlock()
			<-blockA // simulate a hung node
			err := x.SaveState(context.Background(), struct{ Done int }{Done: 8})
			staleWrite <- err
			return err
		}
		if !resumed || st.Done != 7 {
			return errors.New("takeover did not resume from the checkpoint")
		}
		x.SetResult(st)
		return nil
	}}
	// Node A: heartbeat effectively off, so its run goes stale.
	a := e.service("node-a", func(o *tasks.Options) { o.Heartbeat = time.Hour; o.StaleAfter = 100 * time.Millisecond })
	b := e.service("node-b", func(o *tasks.Options) { o.StaleAfter = 100 * time.Millisecond })
	for _, s := range []*tasks.Service{a, b} {
		s.Register("resumable", exec, tasks.TypeOptions{Creatable: true})
	}
	if _, err := a.Create(ctx, tasks.Input{Name: "resumable", Type: "resumable"}); err != nil {
		t.Fatal(err)
	}
	run, err := a.Trigger(ctx, "resumable", "admin")
	if err != nil {
		t.Fatal(err)
	}
	e.dispatch(a)
	for {
		mu.Lock()
		ready := stateA != nil
		mu.Unlock()
		if ready {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A fresh run cannot be taken over.
	// The claim is synchronous, so a wrong takeover would show in the run row
	// as soon as HandleJob returns (the executor itself starts later).
	_ = b.HandleJob(context.Background(), core.Job{Type: tasks.JobType, Payload: json.RawMessage(`{"run_id":"` + run.ID + `"}`)})
	if live, err := b.GetRun(ctx, "resumable", run.ID); err != nil || live.Node != "node-a" || live.Status != tasks.StatusRunning {
		t.Fatalf("live run was taken over: %+v %v", live, err)
	}
	if calls.Load() != 1 {
		t.Fatal("live run was taken over")
	}
	time.Sleep(200 * time.Millisecond)
	// The leader's recovery enqueues a takeover job; node B claims it.
	if _, err := b.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	e.dispatch(b)
	done := e.waitRun(b, "resumable", run.ID, finished)
	if done.Status != tasks.StatusSucceeded || done.Node != "node-b" || string(done.Result) != `{"Done": 7}` {
		t.Fatalf("taken over run = %+v %s", done, done.Result)
	}
	close(blockA) // node A wakes up: its write is fenced off
	select {
	case err := <-staleWrite:
		if !errors.Is(err, tasks.ErrLostRun) {
			t.Fatalf("stale node's checkpoint = %v, want ErrLostRun", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stale node never attempted its write")
	}
	again, _ := b.GetRun(ctx, "resumable", run.ID)
	if again.Status != tasks.StatusSucceeded || string(again.Result) != `{"Done": 7}` {
		t.Fatalf("stale node overwrote the run: %+v", again)
	}
}

// Shutdown hands a running run back; another node resumes it.
func TestShutdownHandsBack(t *testing.T) {
	e := newEnv(t)
	ctx := asAdmin()
	started := make(chan struct{}, 2)
	exec := fakeExec{run: func(ctx context.Context, x *tasks.Exec) error {
		var st struct{ Step int }
		if ok, _ := x.LoadState(&st); ok {
			x.SetResult(st)
			return nil
		}
		if err := x.SaveState(ctx, struct{ Step int }{3}); err != nil {
			return err
		}
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}
	a := e.service("node-a")
	b := e.service("node-b")
	a.Register("job", exec, tasks.TypeOptions{Creatable: true})
	b.Register("job", exec, tasks.TypeOptions{Creatable: true})
	if _, err := a.Create(ctx, tasks.Input{Name: "job", Type: "job"}); err != nil {
		t.Fatal(err)
	}
	run, err := a.Trigger(ctx, "job", "admin")
	if err != nil {
		t.Fatal(err)
	}
	e.dispatch(a)
	<-started
	a.Close(context.Background())
	r, _ := b.GetRun(ctx, "job", run.ID)
	if r.Status != tasks.StatusQueued || !strings.Contains(*r.Log, "shutting down") {
		t.Fatalf("handed-back run = %+v", r)
	}
	e.dispatch(b)
	done := e.waitRun(b, "job", run.ID, finished)
	if done.Status != tasks.StatusSucceeded || string(done.Result) != `{"Step": 3}` || done.Node != "node-b" {
		t.Fatalf("resumed run = %+v %s", done, done.Result)
	}
}

// Each due activation starts exactly one run, even when two nodes tick.
func TestTickStartsDueRunsOnce(t *testing.T) {
	e := newEnv(t)
	ctx := asAdmin()
	now := time.Date(2026, 11, 4, 2, 0, 30, 0, time.UTC)
	clock := func() time.Time { return now }
	release := make(chan struct{})
	exec := fakeExec{run: func(ctx context.Context, _ *tasks.Exec) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	a := e.service("a", func(o *tasks.Options) { o.Now = clock })
	b := e.service("b", func(o *tasks.Options) { o.Now = clock })
	a.Register("w", exec, tasks.TypeOptions{Creatable: true})
	b.Register("w", exec, tasks.TypeOptions{Creatable: true})
	if _, err := a.Create(ctx, tasks.Input{Name: "hourly", Type: "w", Schedule: ptr("0 * * * *")}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Create(ctx, tasks.Input{Name: "manual", Type: "w"}); err != nil {
		t.Fatal(err)
	}
	task, _ := a.Get(ctx, "hourly")
	if want := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC); !task.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at = %v", task.NextRunAt)
	}
	// Not due yet: nothing starts, and the wake-up is the next activation.
	next, err := a.Tick(context.Background(), now)
	if err != nil || !next.Equal(*task.NextRunAt) || len(e.jobs.Jobs()) != 0 {
		t.Fatalf("early tick = %v, %v, %d jobs", next, err, len(e.jobs.Jobs()))
	}
	later := time.Date(2026, 11, 4, 3, 0, 5, 0, time.UTC)
	var wg sync.WaitGroup
	for _, s := range []*tasks.Service{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Tick(context.Background(), later); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	_, active, _ := a.List(ctx)
	if len(active) != 1 || active[0].TaskName != "hourly" || active[0].TriggeredBy != tasks.TriggerSchedule {
		t.Fatalf("active after due tick = %+v", active)
	}
	task, _ = a.Get(ctx, "hourly")
	if want := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC); !task.NextRunAt.Equal(want) {
		t.Fatalf("next_run_at advanced to %v", task.NextRunAt)
	}
	// The next activation while the run is still active is skipped, not queued twice.
	e.dispatch(a)
	if _, err := a.Tick(context.Background(), time.Date(2026, 11, 4, 4, 0, 1, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	_, active, _ = a.List(ctx)
	if len(active) != 1 {
		t.Fatalf("overlapping run queued: %+v", active)
	}
	close(release)
	e.waitRun(a, "hourly", active[0].ID, finished)
	for _, ev := range e.audit.Events() {
		if ev.Action == "task.run" {
			t.Errorf("scheduled run audited as a manual one: %+v", ev)
		}
	}
}

// Policy tasks are created and updated inside the policy's transaction and
// cannot be edited directly.
func TestPolicyTask(t *testing.T) {
	e := newEnv(t)
	svc := e.service("n1")
	svc.Register(tasks.TypeCleanup, fakeExec{run: func(context.Context, *tasks.Exec) error { return nil }}, tasks.TypeOptions{})
	ctx := asAdmin()
	var policyID string
	if err := e.pool.QueryRow(ctx, `INSERT INTO cleanup_policies (name) VALUES ('old-snapshots') RETURNING id::text`).Scan(&policyID); err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, err := tasks.SyncPolicyTask(ctx, tx, tasks.PolicyTask{PolicyID: policyID, PolicyName: "old-snapshots",
		Schedule: ptr("0 2 * * *"), Enabled: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id2, err := tasks.SyncPolicyTask(ctx, tx, tasks.PolicyTask{PolicyID: policyID, PolicyName: "old-snapshots", Enabled: false}, time.Now())
	if err != nil || id2 != id {
		t.Fatalf("resync = %s, %v", id2, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	task, err := svc.Get(ctx, tasks.PolicyTaskName("old-snapshots"))
	if err != nil || task.PolicyID != policyID || task.Enabled || task.Schedule != nil || task.Type != tasks.TypeCleanup {
		t.Fatalf("policy task = %+v, %v", task, err)
	}
	if _, err := svc.Update(ctx, id, tasks.Input{}); !errors.Is(err, tasks.ErrManaged) {
		t.Fatalf("update policy task = %v", err)
	}
	if err := svc.Delete(ctx, id); !errors.Is(err, tasks.ErrManaged) {
		t.Fatalf("delete policy task = %v", err)
	}
	if _, err := e.pool.Exec(ctx, `DELETE FROM cleanup_policies WHERE id = $1`, policyID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, id); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("task outlived its policy: %v", err)
	}
}
