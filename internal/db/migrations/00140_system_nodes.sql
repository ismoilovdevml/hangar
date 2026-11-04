-- +goose Up
-- Heartbeats of the Hangar nodes for the System status screen. Every node
-- upserts its row every few seconds with its role and subsystem checks, and
-- removes it when it stops cleanly; rows silent for an hour are pruned.
CREATE TABLE system_nodes (
    id           text PRIMARY KEY,
    hostname     text NOT NULL,
    url          text NOT NULL,
    version      text NOT NULL,
    commit       text NOT NULL DEFAULT '',
    started_at   timestamptz NOT NULL,
    heartbeat_at timestamptz NOT NULL DEFAULT now(),
    role         text NOT NULL CHECK (role IN ('leader', 'follower')),
    status       text NOT NULL CHECK (status IN ('healthy', 'degraded', 'down')),
    -- [{name, label, status, detail, checked_at, latency_ms, facts}]
    subsystems   jsonb NOT NULL DEFAULT '[]'
);
CREATE INDEX system_nodes_heartbeat_idx ON system_nodes (heartbeat_at);

-- +goose Down
DROP TABLE system_nodes;
