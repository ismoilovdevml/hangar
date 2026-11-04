package tasks

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/tasks/cron"
)

// Tick is the scheduler leader's periodic work (it implements
// scheduler.Ticker): it starts the runs of tasks whose schedule is due,
// re-enqueues runs whose job or node got lost, and fails runs that were
// taken over too often. It returns when it wants to be called again.
//
// Only one node should call Tick at a time (the scheduler holds the leader
// lock), but it stays correct if two do: advancing next_run_at is a
// compare-and-set, so each activation starts at most one run.
func (s *Service) Tick(ctx context.Context, now time.Time) (time.Time, error) {
	ctx = core.WithPrincipal(ctx, SystemPrincipal())
	recErr := s.recover(ctx)
	next, err := s.startDue(ctx, now)
	return next, errors.Join(recErr, err)
}

func (s *Service) startDue(ctx context.Context, now time.Time) (time.Time, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, name, schedule, next_run_at FROM tasks
WHERE enabled AND schedule IS NOT NULL ORDER BY name`)
	if err != nil {
		return time.Time{}, fmt.Errorf("list scheduled tasks: %w", err)
	}
	type due struct {
		id, name, schedule string
		next               *time.Time
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (due, error) {
		var d due
		err := r.Scan(&d.id, &d.name, &d.schedule, &d.next)
		return d, err
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("list scheduled tasks: %w", err)
	}
	var earliest time.Time
	consider := func(t time.Time) {
		if !t.IsZero() && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
		}
	}
	var errs []error
	for _, d := range list {
		sched, err := cron.Parse(d.schedule)
		if err != nil {
			s.log.Warn("tasks: invalid schedule; task not scheduled", "task", d.name, "schedule", d.schedule, "error", err)
			continue
		}
		upcoming := sched.Next(now)
		switch {
		case d.next == nil:
			if _, err := s.pool.Exec(ctx, `UPDATE tasks SET next_run_at = $2 WHERE id = $1 AND next_run_at IS NULL`, d.id, upcoming); err != nil {
				errs = append(errs, fmt.Errorf("schedule task %q: %w", d.name, err))
			}
			consider(upcoming)
		case now.Before(*d.next):
			consider(*d.next)
		default:
			tag, err := s.pool.Exec(ctx, `UPDATE tasks SET next_run_at = $2 WHERE id = $1 AND next_run_at = $3`, d.id, upcoming, *d.next)
			if err != nil {
				errs = append(errs, fmt.Errorf("schedule task %q: %w", d.name, err))
				continue
			}
			consider(upcoming)
			if tag.RowsAffected() == 0 {
				continue // another leader advanced it
			}
			run, err := s.Trigger(ctx, d.id, TriggerSchedule)
			switch {
			case errors.Is(err, ErrActiveRun):
				s.log.Info("tasks: scheduled run skipped; the previous run is still active", "task", d.name)
			case err != nil:
				errs = append(errs, fmt.Errorf("start scheduled run of %q: %w", d.name, err))
			default:
				s.log.Info("tasks: scheduled run queued", "task", d.name, "run_id", run.ID, "next_run_at", upcoming)
			}
		}
	}
	return earliest, errors.Join(errs...)
}

// recover re-enqueues lost runs and fails abandoned ones.
func (s *Service) recover(ctx context.Context) error {
	stale := s.opts.StaleAfter.Seconds()
	var errs []error

	// Queued runs whose job never reached a node (queue dropped it after
	// retries while every node was busy, or the enqueue was lost).
	rows, err := s.pool.Query(ctx, `UPDATE task_runs SET heartbeat_at = now()
WHERE status = 'queued' AND heartbeat_at < now() - make_interval(secs => $1)
RETURNING id::text, extract(epoch FROM heartbeat_at)::bigint`, stale)
	if err == nil {
		type q struct {
			id string
			at int64
		}
		var list []q
		list, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (q, error) {
			var v q
			err := r.Scan(&v.id, &v.at)
			return v, err
		})
		for _, v := range list {
			if eerr := s.enqueue(ctx, v.id, "requeue-"+strconv.FormatInt(v.at, 10)); eerr != nil {
				errs = append(errs, fmt.Errorf("re-enqueue run %s: %w", v.id, eerr))
			} else {
				s.log.Info("tasks: re-enqueued a queued run that no node picked up", "run_id", v.id)
			}
		}
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("find queued runs: %w", err))
	}

	// Running runs whose node stopped heart-beating: enqueue a job so another
	// node takes them over (claim checks the staleness again).
	rows, err = s.pool.Query(ctx, `SELECT id::text, claims FROM task_runs
WHERE status = 'running' AND heartbeat_at < now() - make_interval(secs => $1) AND recoveries < $2`, stale, s.opts.MaxRecoveries)
	if err == nil {
		type q struct {
			id     string
			claims int
		}
		var list []q
		list, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (q, error) {
			var v q
			err := r.Scan(&v.id, &v.claims)
			return v, err
		})
		for _, v := range list {
			if eerr := s.enqueue(ctx, v.id, "takeover-"+strconv.Itoa(v.claims)); eerr != nil {
				errs = append(errs, fmt.Errorf("enqueue takeover of run %s: %w", v.id, eerr))
			} else {
				s.log.Warn("tasks: run's node stopped responding; enqueued a takeover", "run_id", v.id)
			}
		}
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("find stale runs: %w", err))
	}

	// Runs taken over too often: something kills every node that runs them.
	rows, err = s.pool.Query(ctx, `
WITH u AS (
	UPDATE task_runs SET status = 'failed', finished_at = now(),
		message = 'Abandoned: the run stopped responding ' || recoveries || ' times'
	WHERE status = 'running' AND heartbeat_at < now() - make_interval(secs => $1) AND recoveries >= $2
	RETURNING *
)
SELECT u.id::text, u.task_id::text, t.name, t.type, u.status, u.progress, u.message, u.triggered_by, u.node,
	u.cancel_requested, u.created_at, u.started_at, u.finished_at, u.result
FROM u JOIN tasks t ON t.id = u.task_id`, stale, s.opts.MaxRecoveries)
	if err == nil {
		var failed []*Run
		failed, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (*Run, error) { return scanRun(r) })
		for _, r := range failed {
			s.log.Error("tasks: run abandoned", "task", r.TaskName, "run_id", r.ID, "message", r.Message)
			s.publishFinished(ctx, r)
		}
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("fail abandoned runs: %w", err))
	}
	return errors.Join(errs...)
}
