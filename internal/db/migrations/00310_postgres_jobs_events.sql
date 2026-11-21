-- +goose Up
-- The job queue, formerly a NATS JetStream work queue (ADR 0011). One row
-- per job: queued until run_at, running while a node holds its lease
-- (locked_until, renewed by the node's heartbeat), then deleted, or kept
-- as done for the deduplication window, or dead after its last attempt.
-- Workers claim rows with FOR UPDATE SKIP LOCKED; a running row whose lease
-- expired (its node died) is claimed again by another node.
CREATE TABLE jobs (
    id           text PRIMARY KEY,
    type         text NOT NULL CHECK (type <> ''),
    -- NULL: no deduplication. Otherwise, without coalescing, a job with the
    -- same type and key enqueued within the deduplication window is stored
    -- once; with coalescing, at most one job per type and key waits.
    key          text,
    coalescing   boolean NOT NULL DEFAULT false,
    payload      jsonb,
    state        text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'done', 'dead')),
    attempts     integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    run_at       timestamptz NOT NULL DEFAULT now(),
    enqueued_at  timestamptz NOT NULL DEFAULT now(),
    locked_by    text,
    locked_until timestamptz,
    started_at   timestamptz,
    finished_at  timestamptz,
    last_error   text NOT NULL DEFAULT ''
);
-- At most one waiting job per type and key: coalescing enqueues merge into it.
CREATE UNIQUE INDEX jobs_waiting_key_idx ON jobs (type, key) WHERE state = 'queued' AND key IS NOT NULL;
CREATE INDEX jobs_claim_idx ON jobs (type, run_at) WHERE state = 'queued';
CREATE INDEX jobs_lease_idx ON jobs (type, locked_until) WHERE state = 'running';
CREATE INDEX jobs_key_idx ON jobs (type, key, enqueued_at) WHERE key IS NOT NULL;
CREATE INDEX jobs_finished_idx ON jobs (finished_at) WHERE state IN ('done', 'dead');

-- Event payloads too large for a NOTIFY (8000 bytes): the notification
-- carries the row id and receivers read the event here. Rows are pruned a
-- few minutes after they were written.
CREATE TABLE event_payloads (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payload    text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX event_payloads_created_idx ON event_payloads (created_at);

-- The key nodes sign their requests to each other with (System logs of
-- another node). A single row, created by the first node that needs it.
CREATE TABLE cluster_node_key (
    id         boolean PRIMARY KEY DEFAULT true CHECK (id),
    key        bytea NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE cluster_node_key;
DROP TABLE event_payloads;
DROP TABLE jobs;
