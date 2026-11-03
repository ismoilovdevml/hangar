# 0004. NATS JetStream for events and jobs, embedded or clustered

Date: 2026-11-03 · Status: Accepted

## Context
Hangar needs a live event feed (UI over SSE, cache invalidation), a durable job
queue (scans, cleanup, compaction, replication) shared by all nodes, and a
replication stream to edge sites. Single-node installs must not need extra
services.

## Decision
- [NATS](https://nats.io) with JetStream behind two small interfaces:
  `core.EventBus` (publish/subscribe, at-most-once, subjects
  `hangar.events.<type>`) and `core.JobQueue` (durable work queue, stream
  `HANGAR_JOBS`, one durable consumer per job type, retry with backoff,
  dedup by job key within a window).
- Single node: the NATS server runs inside the hangar process
  (`nats-server` as a Go library), JetStream state under `nats.data_dir`,
  in-process connection without a network listener unless `nats.listen` is set.
- Cluster: `nats.embedded=false`, `nats.url` points at an external NATS
  cluster; stream replicas configurable.
- Leader-only work (schedulers) uses a PostgreSQL advisory lock, not NATS.

## Consequences
- `docker run hangar` works with only PostgreSQL; HA swaps configuration, not code.
- Job handlers must be idempotent (at-least-once delivery).
- In-memory fakes (`coretest.EventBus`, `coretest.JobQueue`) keep unit tests
  free of NATS.
