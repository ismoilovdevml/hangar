package scanning

import (
	"context"
	"errors"
	"fmt"

	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// TaskType is the task type of the periodic rescan (tasks.Service).
const TaskType = "rescan"

// RescanTask is the built-in task that rechecks components against the
// latest advisories. Hourly runs pick up what arrived without an arrival
// scan, retry failed scans and rescan every component whose last scan is
// older than scanning.rescan_interval.
var RescanTask = tasks.Builtin{
	Name: "vulnerability-rescan", Type: TaskType, Schedule: "20 * * * *",
	Description: "Check packages against the latest OSV advisories: new, failed and stale scans",
}

// rescanState is the checkpoint of a rescan run.
type rescanState struct {
	After string  `json:"after"`
	Done  int64   `json:"done"`
	Sum   Summary `json:"summary"`
}

// rescanResult is the summary stored with a finished run.
type rescanResult struct {
	Summary
	Due            int64 `json:"due"`
	WaiversExpired int   `json:"waivers_expired"`
}

// Executor returns the executor of TaskType.
func (s *Service) Executor() tasks.Executor {
	return tasks.Func(s.rescan)
}

func (s *Service) rescan(ctx context.Context, x *tasks.Exec) error {
	if !s.opts.Enabled {
		x.Logf("Vulnerability scanning is turned off (scanning.enabled = false); nothing to do")
		x.Progress(1, "Scanning is turned off")
		x.SetResult(rescanResult{})
		return nil
	}
	expired, err := s.ReopenExpired(ctx)
	if err != nil {
		return err
	}
	if expired > 0 {
		x.Logf("%d expired waivers re-opened", expired)
	}
	var st rescanState
	if _, err := x.LoadState(&st); err != nil {
		return err
	}
	due, err := s.store.countDue(ctx, s.opts.RescanInterval, s.opts.RetryFailedAfter)
	if err != nil {
		return err
	}
	total := st.Done + due
	x.Logf("%d components to scan (never scanned, failed, or last scanned more than %s ago)", due, s.opts.RescanInterval)
	var lastErr error
	for {
		batch, err := s.store.dueTargets(ctx, s.opts.RescanInterval, s.opts.RetryFailedAfter, st.After, batchSize)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		sum, err := s.scan(ctx, batch)
		st.Sum.add(sum)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if sum.Failed == 0 {
				return err // could not record the outcome
			}
			lastErr = err
			x.Warnf("%d components could not be scanned: %v", sum.Failed, err)
		}
		st.After = batch[len(batch)-1].ComponentID
		st.Done += int64(len(batch))
		x.Progress(fraction(st.Done, total), fmt.Sprintf("Scanned %d of %d components, %d findings", st.Done, total, st.Sum.Findings))
		if err := x.SaveState(ctx, st); err != nil {
			return err
		}
	}
	res := rescanResult{Summary: st.Sum, Due: total, WaiversExpired: expired}
	x.SetResult(res)
	msg := fmt.Sprintf("Scanned %d components: %d findings (%d new), %d failed", st.Sum.Scanned, st.Sum.Findings, st.Sum.New, st.Sum.Failed)
	if st.Sum.Quarantined+st.Sum.Released > 0 {
		msg += fmt.Sprintf(", %d quarantined, %d released", st.Sum.Quarantined, st.Sum.Released)
	}
	x.Progress(1, msg)
	x.Logf("%s", msg)
	if st.Sum.Scanned == 0 && st.Sum.Failed > 0 {
		return errors.Join(errors.New("no component could be scanned; is the advisory database reachable?"), lastErr)
	}
	return nil
}

func fraction(done, total int64) float64 {
	if total <= 0 {
		return 1
	}
	return min(float64(done)/float64(total), 1)
}
