package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
	"github.com/ismoilovdevml/hangar/internal/tasks"
)

// maxConsecutiveFailures aborts a run when deletions keep failing (database
// or storage outage) instead of grinding through every candidate.
const maxConsecutiveFailures = 50

// RunResult is the summary stored with a finished run.
type RunResult struct {
	Policy      string    `json:"policy"`
	EvaluatedAt time.Time `json:"evaluated_at"`
	Deleted     int64     `json:"deleted"`
	Bytes       int64     `json:"bytes"`
	Failed      int64     `json:"failed"`
	// Gone counts candidates deleted by someone else during the run.
	Gone         int64         `json:"gone"`
	Held         int64         `json:"held"`
	Repositories []RepoSummary `json:"repositories"`
}

// checkpoint is the resumable state of a run.
type checkpoint struct {
	Pos    position  `json:"pos"`
	Result RunResult `json:"result"`
}

type executor struct{ s *Service }

// Executor returns the tasks.Executor for tasks.TypeCleanup.
func (s *Service) Executor() tasks.Executor { return executor{s} }

// Validate implements tasks.Executor. Policy tasks are configured by the
// server ({"policy_id": ...}).
func (executor) Validate(cfg json.RawMessage) error {
	var c struct {
		PolicyID string `json:"policy_id"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil || !isUUID(c.PolicyID) {
		return fmt.Errorf("cleanup task config needs policy_id: %w", core.ErrInvalid)
	}
	return nil
}

// Execute implements tasks.Executor: it deletes the policy's candidates in
// batches, checkpointing after every batch of packages.
func (e executor) Execute(ctx context.Context, x *tasks.Exec) error {
	s := e.s
	policyID := x.Task.PolicyID
	if policyID == "" {
		var c struct {
			PolicyID string `json:"policy_id"`
		}
		_ = json.Unmarshal(x.Task.Config, &c)
		policyID = c.PolicyID
	}
	p, err := s.get(ctx, s.pool, policyID, false)
	if err != nil {
		return err
	}
	in := Input{Name: p.Name, Format: p.Format, Criteria: p.Criteria, Repositories: p.Repositories}
	r, repos, err := s.plan(ctx, &in)
	if err != nil {
		return err
	}

	var cp checkpoint
	resumed, err := x.LoadState(&cp)
	if err != nil {
		return err
	}
	if !resumed {
		// Evaluate the whole run at one instant, also across resumptions.
		cp.Result = RunResult{Policy: p.Name, EvaluatedAt: s.opts.Now().UTC()}
		x.Logf("Policy %s: cleaning %d repositories %v", p.Name, len(repos), names(repos))
	}
	res := &cp.Result
	repoSum := func(name string) *RepoSummary {
		for i := range res.Repositories {
			if res.Repositories[i].Repository == name {
				return &res.Repositories[i]
			}
		}
		res.Repositories = append(res.Repositories, RepoSummary{Repository: name})
		return &res.Repositories[len(res.Repositories)-1]
	}
	failures := 0
	err = s.scan(ctx, r, repos, res.EvaluatedAt, cp.Pos, scanHooks{
		candidate: func(repo *core.Repository, c Candidate) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			err := s.opts.Components.DeleteComponent(ctx, repo, c.ID)
			switch {
			case err == nil:
				failures = 0
				res.Deleted++
				res.Bytes += c.SizeBytes
				rs := repoSum(repo.Name)
				rs.ComponentCount++
				rs.Bytes += c.SizeBytes
				x.Logf("Deleted %s/%s (%d bytes)", repo.Name, c.Coordinates(), c.SizeBytes)
			case errors.Is(err, core.ErrNotFound):
				res.Gone++
			case ctx.Err() != nil:
				return ctx.Err()
			default:
				failures++
				res.Failed++
				x.Logf("Could not delete %s/%s: %v", repo.Name, c.Coordinates(), err)
				if failures >= maxConsecutiveFailures {
					return fmt.Errorf("%d deletions in a row failed, stopping (last: %w)", failures, err)
				}
			}
			return nil
		},
		held: func(*core.Repository) { res.Held++ },
		batch: func(pos position, frac float64) error {
			cp.Pos = pos
			x.Progress(frac, progressMessage(res))
			return x.SaveState(ctx, cp)
		},
	})
	x.SetResult(res)
	if err != nil {
		return err
	}
	msg := progressMessage(res)
	x.Progress(1, msg)
	x.Logf("%s", msg)
	s.audit(ctx, "cleanup.run", p.Name, map[string]any{"run_id": x.Run.ID, "deleted": res.Deleted, "bytes": res.Bytes,
		"failed": res.Failed, "held": res.Held})
	if res.Failed > 0 {
		return fmt.Errorf("%s; %d deletions failed (see the run log)", msg, res.Failed)
	}
	return nil
}

func progressMessage(r *RunResult) string {
	msg := fmt.Sprintf("Deleted %d components (%s)", r.Deleted, humanBytes(r.Bytes))
	if r.Held > 0 {
		msg += fmt.Sprintf(", %d kept for quarantine or legal hold", r.Held)
	}
	return msg
}

func names(repos []*core.Repository) []string {
	out := make([]string, len(repos))
	for i, r := range repos {
		out[i] = r.Name
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
