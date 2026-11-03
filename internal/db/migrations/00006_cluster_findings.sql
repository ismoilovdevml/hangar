-- +goose Up
-- Cluster nodes, replication, and vulnerability/licence findings.

CREATE TABLE nodes (
    id             uuid PRIMARY KEY,
    name           text NOT NULL,
    address        text NOT NULL,
    role           text NOT NULL DEFAULT 'follower' CHECK (role IN ('leader', 'follower', 'edge')),
    zone           text NOT NULL DEFAULT '',
    version        text NOT NULL DEFAULT '',
    started_at     timestamptz NOT NULL DEFAULT now(),
    last_heartbeat timestamptz NOT NULL DEFAULT now(),
    -- cpu, memory, cache disk, req/s ... reported with each heartbeat.
    stats          jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX nodes_heartbeat_idx ON nodes (last_heartbeat);

CREATE TABLE replication_targets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL UNIQUE,
    url         text NOT NULL,
    kind        text NOT NULL DEFAULT 'edge' CHECK (kind IN ('edge', 'dr')),
    -- Repository name patterns to replicate.
    repos       text[] NOT NULL DEFAULT '{}',
    config      jsonb NOT NULL DEFAULT '{}',
    enabled     boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE replication_state (
    target_id       uuid NOT NULL REFERENCES replication_targets (id) ON DELETE CASCADE,
    repo_id         uuid NOT NULL REFERENCES repositories (id) ON DELETE CASCADE,
    -- Last applied event-stream sequence.
    cursor          bigint NOT NULL DEFAULT 0,
    lag_seconds     integer NOT NULL DEFAULT 0,
    objects_today   bigint NOT NULL DEFAULT 0,
    last_error      text,
    last_success_at timestamptz,
    retry_at        timestamptz,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (target_id, repo_id)
);

CREATE TABLE findings (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    component_id  uuid NOT NULL REFERENCES components (id) ON DELETE CASCADE,
    kind          text NOT NULL DEFAULT 'vulnerability' CHECK (kind IN ('vulnerability', 'license')),
    advisory_id   text NOT NULL,
    title         text NOT NULL DEFAULT '',
    severity      text NOT NULL CHECK (severity IN ('critical', 'high', 'medium', 'low', 'unknown')),
    cvss          numeric(3, 1) CHECK (cvss IS NULL OR (cvss >= 0 AND cvss <= 10)),
    package       text NOT NULL,
    installed     text NOT NULL DEFAULT '',
    fixed_in      text,
    status        text NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open', 'assigned', 'waived', 'fixed')),
    waiver_reason text,
    waiver_until  timestamptz,
    waived_by     text,
    assignee      text,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (component_id, advisory_id, package),
    CHECK (status <> 'waived' OR (waiver_reason IS NOT NULL AND waiver_until IS NOT NULL))
);
CREATE INDEX findings_severity_idx ON findings (severity, status);
CREATE INDEX findings_advisory_idx ON findings (advisory_id);

-- +goose Down
DROP TABLE findings;
DROP TABLE replication_state;
DROP TABLE replication_targets;
DROP TABLE nodes;
