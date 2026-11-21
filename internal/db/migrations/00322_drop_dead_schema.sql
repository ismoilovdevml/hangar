-- +goose Up
-- Schema nothing reads or writes any more:
-- - nodes: replaced by system_nodes (00140).
-- - replication_targets, replication_state: edge replication was never
--   built; its API answers 501.
-- - task_runs.node_id: runs record their node in task_runs.node (00060).
-- Indexes another index already serves:
-- - components_created_idx (created_at DESC): components_created_id_idx
--   (created_at DESC, id DESC) has the same leading column.
-- - assets_path_prefix_idx (repo_id, path text_pattern_ops): prefix scans
--   use assets_repo_path_c_idx (repo_id, path COLLATE "C"), which PostgreSQL
--   also uses for path LIKE 'prefix%' in any database collation.
-- - findings_component_idx (component_id): the unique index
--   findings_component_advisory_package_location_key leads with component_id.
DROP TABLE replication_state;
DROP TABLE replication_targets;
DROP TABLE nodes;
ALTER TABLE task_runs DROP COLUMN node_id;
DROP INDEX components_created_idx;
DROP INDEX assets_path_prefix_idx;
DROP INDEX findings_component_idx;

-- +goose Down
CREATE INDEX findings_component_idx ON findings (component_id);
CREATE INDEX assets_path_prefix_idx ON assets (repo_id, path text_pattern_ops);
CREATE INDEX components_created_idx ON components (created_at DESC);
ALTER TABLE task_runs ADD COLUMN node_id uuid;

CREATE TABLE nodes (
    id             uuid PRIMARY KEY,
    name           text NOT NULL,
    address        text NOT NULL,
    role           text NOT NULL DEFAULT 'follower' CHECK (role IN ('leader', 'follower', 'edge')),
    zone           text NOT NULL DEFAULT '',
    version        text NOT NULL DEFAULT '',
    started_at     timestamptz NOT NULL DEFAULT now(),
    last_heartbeat timestamptz NOT NULL DEFAULT now(),
    stats          jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX nodes_heartbeat_idx ON nodes (last_heartbeat);

CREATE TABLE replication_targets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    url         text NOT NULL,
    kind        text NOT NULL DEFAULT 'edge' CHECK (kind IN ('edge', 'dr')),
    repos       text[] NOT NULL DEFAULT '{}',
    config      jsonb NOT NULL DEFAULT '{}',
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE replication_state (
    target_id       uuid NOT NULL REFERENCES replication_targets (id) ON DELETE CASCADE,
    repo_id         uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    cursor          bigint NOT NULL DEFAULT 0,
    lag_seconds     integer NOT NULL DEFAULT 0,
    objects_today   bigint NOT NULL DEFAULT 0,
    last_error      text,
    last_success_at timestamptz,
    retry_at        timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_id, repo_id)
);
