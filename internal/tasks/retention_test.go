package tasks_test

import (
	"context"
	"testing"
	"time"
)

func TestPruneRuns(t *testing.T) {
	e := newEnv(t)
	svc := e.service("n1")
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	var busy, rare string
	if err := e.pool.QueryRow(ctx, `INSERT INTO tasks (name, type) VALUES ('busy', 'x') RETURNING id::text`).Scan(&busy); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `INSERT INTO tasks (name, type) VALUES ('rare', 'x') RETURNING id::text`).Scan(&rare); err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	now := time.Now()
	run := func(task, status string, age time.Duration) {
		t.Helper()
		var fin any
		if status != "queued" && status != "running" {
			fin = now.Add(-age)
		}
		exec(`INSERT INTO task_runs (task_id, status, created_at, finished_at)
			VALUES ($1, $2, $3, $4), ($1, $2, $3::timestamptz + interval '1 second', $4)`,
			task, status, now.Add(-age-time.Minute), fin)
	}
	// busy: 2 x 3 old finished runs (pruned), 2 recent ones and an old
	// running one (kept: active runs are never pruned).
	run(busy, "succeeded", 200*day)
	run(busy, "failed", 150*day)
	run(busy, "cancelled", 100*day)
	run(busy, "succeeded", day)
	exec(`INSERT INTO task_runs (task_id, status, created_at) VALUES ($1, 'running', $2)`, busy, now.Add(-120*day))
	// rare: only old runs; its newest one is kept.
	run(rare, "succeeded", 300*day)

	count := func(task string) int {
		t.Helper()
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM task_runs WHERE task_id = $1`, task).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n, err := svc.PruneRuns(ctx, 0); err != nil || n != 0 {
		t.Fatalf("retention 0 pruned %d (err=%v)", n, err)
	}
	n, err := svc.PruneRuns(ctx, 90*day)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("pruned %d runs, want 7", n)
	}
	if got := count(busy); got != 3 {
		t.Errorf("busy keeps %d runs, want the 2 recent and the running one", got)
	}
	if got := count(rare); got != 1 {
		t.Errorf("rare keeps %d runs, want its newest", got)
	}
	if n, err := svc.PruneRuns(ctx, 90*day); err != nil || n != 0 {
		t.Errorf("second prune deleted %d (err=%v)", n, err)
	}
}
