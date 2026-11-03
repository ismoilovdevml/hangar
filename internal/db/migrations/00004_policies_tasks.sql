-- +goose Up
-- Cleanup policies, scheduled tasks and their runs.

CREATE TABLE cleanup_policies (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    -- NULL = any format.
    format      text,
    -- {"olderThanDays":90,"notDownloadedDays":60,"keepNewest":5,"keepMatching":["release-*"],"prereleaseOnly":false}
    criteria    jsonb NOT NULL DEFAULT '{}',
    -- Cron expression (5 fields, UTC); NULL = only run manually.
    schedule    text,
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE policy_repos (
    policy_id uuid NOT NULL REFERENCES cleanup_policies (id) ON DELETE CASCADE,
    repo_id   uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    PRIMARY KEY (policy_id, repo_id)
);
CREATE INDEX policy_repos_repo_idx ON policy_repos (repo_id);

CREATE TABLE tasks (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    -- cleanup, blob_compaction, docker_gc, rescan, reindex, directory_sync,
    -- db_backup, replication, proxy_metadata_refresh, ...
    type        text NOT NULL,
    description text NOT NULL DEFAULT '',
    schedule    text,
    config      jsonb NOT NULL DEFAULT '{}',
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE task_runs (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id     uuid NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
    status      text NOT NULL DEFAULT 'queued'
                CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    progress    real NOT NULL DEFAULT 0 CHECK (progress >= 0 AND progress <= 1),
    message     text NOT NULL DEFAULT '',
    log         text NOT NULL DEFAULT '',
    triggered_by text NOT NULL DEFAULT 'schedule',
    node_id     uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz,
    finished_at timestamptz
);
CREATE INDEX task_runs_task_idx ON task_runs (task_id, created_at DESC);
-- At most one active run per task.
CREATE UNIQUE INDEX task_runs_one_active_idx ON task_runs (task_id) WHERE status IN ('queued', 'running');

-- +goose Down
DROP TABLE task_runs;
DROP TABLE tasks;
DROP TABLE policy_repos;
DROP TABLE cleanup_policies;
