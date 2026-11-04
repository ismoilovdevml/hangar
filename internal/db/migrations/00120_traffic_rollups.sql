-- +goose Up
-- Per-minute traffic counters behind the Overview screen. Every server
-- process (instance) adds the increments of its own Prometheus counters to
-- the row of the minute they happened in, so the numbers survive restarts and
-- cover every node of a cluster (queries sum over instances). Minutes without
-- traffic have no row. Rows older than the retention are deleted by the
-- server.
--
-- Latency histograms are cumulative counts: element i counts the requests
-- that took at most the i-th bound of the fixed bounds defined in
-- internal/rollup, and the last element counts every request.

CREATE TABLE traffic_minutes (
    minute          timestamptz NOT NULL,
    instance        uuid        NOT NULL,
    -- Requests served (probes, /metrics and the event stream excluded).
    requests        bigint      NOT NULL DEFAULT 0 CHECK (requests >= 0),
    -- Successful GETs of format content (package downloads and pulls).
    downloads       bigint      NOT NULL DEFAULT 0 CHECK (downloads >= 0),
    -- Proxy content requests answered from the local cache / from the remote,
    -- counted once per client request even when it went through a group.
    cache_hits      bigint      NOT NULL DEFAULT 0 CHECK (cache_hits >= 0),
    cache_misses    bigint      NOT NULL DEFAULT 0 CHECK (cache_misses >= 0),
    cache_hit_bytes bigint      NOT NULL DEFAULT 0 CHECK (cache_hit_bytes >= 0),
    latency         bigint[]    NOT NULL DEFAULT '{}',
    PRIMARY KEY (minute, instance)
);

-- Proxy remote latency (time to response headers) per proxy repository.
CREATE TABLE upstream_minutes (
    minute     timestamptz NOT NULL,
    instance   uuid        NOT NULL,
    repository text        NOT NULL,
    latency    bigint[]    NOT NULL DEFAULT '{}',
    PRIMARY KEY (minute, instance, repository)
);

-- Single row: when traffic recording started on this database. Windows that
-- reach back further are reported as covered from this time on.
CREATE TABLE traffic_coverage (
    id    boolean     PRIMARY KEY DEFAULT true CHECK (id),
    since timestamptz NOT NULL
);

-- +goose Down
DROP TABLE traffic_coverage;
DROP TABLE upstream_minutes;
DROP TABLE traffic_minutes;
