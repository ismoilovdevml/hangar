-- +goose Up
-- Task runtime (scheduling, run ownership, checkpoints, results) and
-- cleanup policy previews.

-- builtin: created by the server at startup (blob compaction, docker GC, ...);
--   schedule and enabled may be edited, the task cannot be deleted.
-- policy_id: the task that runs a cleanup policy; managed with the policy.
-- next_run_at: when the scheduler leader starts the next run (NULL until
--   first computed, or when the task has no schedule).
ALTER TABLE tasks
    ADD COLUMN builtin     boolean NOT NULL DEFAULT false,
    ADD COLUMN policy_id   uuid UNIQUE REFERENCES cleanup_policies (id) ON DELETE CASCADE,
    ADD COLUMN next_run_at timestamptz;
CREATE INDEX tasks_due_idx ON tasks (next_run_at) WHERE enabled AND schedule IS NOT NULL;

-- node: host name of the node executing (or last executing) the run.
-- heartbeat_at: refreshed every few seconds by the executing node; a running
--   run whose heartbeat is stale is taken over by another node.
-- claims: incremented every time a node claims the run; it fences writes of
--   a node that lost the run to a takeover.
-- recoveries: takeovers of a running run after its node stopped heart-beating.
-- state: checkpoint of a resumable run; result: summary of a finished run.
ALTER TABLE task_runs
    ADD COLUMN node             text NOT NULL DEFAULT '',
    ADD COLUMN heartbeat_at     timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN cancel_requested boolean NOT NULL DEFAULT false,
    ADD COLUMN claims           integer NOT NULL DEFAULT 0,
    ADD COLUMN recoveries       integer NOT NULL DEFAULT 0,
    ADD COLUMN state            jsonb,
    ADD COLUMN result           jsonb;
CREATE INDEX task_runs_active_heartbeat_idx ON task_runs (heartbeat_at) WHERE status IN ('queued', 'running');

-- A cleanup policy is saved only after its exact criteria were previewed.
-- Only the SHA-256 of the token handed to the client is stored.
CREATE TABLE cleanup_previews (
    token_hash  text PRIMARY KEY,
    fingerprint text NOT NULL,
    actor       text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);
CREATE INDEX cleanup_previews_expires_idx ON cleanup_previews (expires_at);

-- +goose Down
DROP TABLE cleanup_previews;
DROP INDEX task_runs_active_heartbeat_idx;
ALTER TABLE task_runs
    DROP COLUMN result,
    DROP COLUMN state,
    DROP COLUMN recoveries,
    DROP COLUMN claims,
    DROP COLUMN cancel_requested,
    DROP COLUMN heartbeat_at,
    DROP COLUMN node;
DROP INDEX tasks_due_idx;
ALTER TABLE tasks
    DROP COLUMN next_run_at,
    DROP COLUMN policy_id,
    DROP COLUMN builtin;
