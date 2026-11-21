package tasks

import (
	"context"
	"fmt"
	"time"
)

// pruneBatch is the number of runs PruneRuns deletes per statement, so no
// single statement holds row locks on, or writes WAL for, the whole backlog
// of a first prune.
const pruneBatch = 1000

// PruneRuns deletes finished runs (succeeded, failed or cancelled) that
// ended more than retention ago, in batches, and returns how many it
// deleted. The newest run of every task is kept whatever its age, so a
// task's last result stays visible. Zero or negative retention keeps
// everything. Active runs are never touched.
func (s *Service) PruneRuns(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, `
DELETE FROM task_runs WHERE id IN (
	SELECT r.id FROM task_runs r
	 WHERE r.status IN ('succeeded', 'failed', 'cancelled')
	   AND coalesce(r.finished_at, r.created_at) < now() - make_interval(secs => $1)
	   AND EXISTS (SELECT 1 FROM task_runs n WHERE n.task_id = r.task_id AND n.created_at > r.created_at)
	 LIMIT $2)`, retention.Seconds(), pruneBatch)
		if err != nil {
			return total, fmt.Errorf("prune task runs: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < pruneBatch {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
