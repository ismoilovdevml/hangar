# 0011. PostgreSQL for events and jobs; NATS removed

Date: 2026-11-21 · Status: Accepted · Supersedes [0004](0004-nats-jetstream.md)

## Context
ADR 0004 put the event bus and the job queue on NATS JetStream: embedded in
the process for one node, an external NATS cluster for several. In practice:

- Every installation already depends on PostgreSQL; NATS was a second
  stateful system to deploy, secure, back up and upgrade (a Helm subchart,
  a compose service, `nats.data_dir` on disk), and the embedded server tied
  single-node installs to local disk.
- Leader election, locks, task runs and every piece of state already lived
  in PostgreSQL; JetStream held only transient work.
- APT/RPM metadata regeneration and npm package state cleanup ran as
  `go fn(context.Background())` on every node for every `component.*`
  event: N nodes x M events goroutines, all lost when NATS dropped.
- Dead JetStream consumers (blob GC, auth purge, audit partitions) and an
  unused scheduler `Entries` mechanism had survived the move to tasks.

## Decision
Events and jobs move to PostgreSQL (package `internal/events`); NATS and the
`nats-server` / `nats.go` modules are removed. `core.EventBus` and
`core.JobQueue` are unchanged for their callers.

**Event bus: LISTEN/NOTIFY.** `Publish` delivers to the subscribers of the
publishing node in process, then `pg_notify` on channel `hangar_events`; the
other nodes receive it on one dedicated listener connection per node (outside
the pool, `application_name = hangar-listener`) and skip their own
notifications. Payloads above 7900 bytes (NOTIFY refuses 8000) are stored in
`event_payloads` and the notification carries the row id; rows are pruned
after 10 minutes. Delivery to other nodes is at-most-once. When the listener
reconnects (backoff 0.5 s doubling to 30 s, jittered; a keepalive ping every
30 s detects dead connections), or a subscriber's queue of 1024 events
overflows, every subscription receives `core.EventResync`: caches drop
everything, SSE streams end so clients reload. Each subscription has its own
goroutine. Schemas other than `public` get channels suffixed with a hash of
the schema, so installations sharing a database do not hear each other.

**Job queue: the `jobs` table.** Workers claim with `FOR UPDATE SKIP LOCKED`
under a lease (`locked_until`, default 10 minutes) that the running node
renews every lease/3; a job whose node died is claimed again after its lease
expires; a node that finds its lease gone cancels the run. Semantics kept
from v0.1.9: one worker per job type per node, one job at a time; the run
deadline is `max(10m, 2 x (scanner.timeout + 1m))`; failures retry after
5 s, 10 s, 20 s ... (capped at 10 minutes) up to 5 attempts, then the job is
`dead` (kept 7 days); `Close` cancels running handlers, waits up to 15 s and
returns interrupted jobs to the queue without counting the attempt. A NOTIFY
on `hangar_jobs` wakes idle workers; they poll every 5 s as a fallback and
sleep no longer than the next delayed job or lease expiry. `Job.Key`
deduplicates for 10 minutes as before. New `Job.Debounce` coalesces: at most
one waiting job per type and key (unique partial index), run no sooner than
the debounce after the first enqueue, never two of one key at once; a job
enqueued while one runs waits for a run of its own.

**Metadata regeneration** becomes durable coalesced jobs `apt.reindex`,
`rpm.reindex` (key = repository id) and `npm.drop_state` (key = repository
id and package), debounced 2 s: every node still hears the event and
enqueues, the queue keeps one job, one node runs it. Because the publishing
node delivers in process, a NOTIFY lost elsewhere loses no regeneration.

**System logs of other nodes** go over HTTP: `POST
/internal/v1/system/logs` on the URL each node publishes in `system_nodes`
(`server.node_url`, default: the local address of the route to the database
with the port of `server.addr`), signed with HMAC-SHA256 over a key in
`cluster_node_key` (60 s clock skew).

**Configuration.** `nats.*` keys still load and are ignored, with one warning
at startup.

## Consequences
- One stateful dependency: PostgreSQL. Nothing to configure for HA beyond
  several nodes sharing the database; JetStream state and `nats.data_dir`
  can be deleted after the upgrade.
- The listener needs a session-mode connection: LISTEN does not work through
  a transaction-pooling proxy (PgBouncer `pool_mode=transaction`), the same
  constraint as advisory locks already had.
- Each node holds one more database connection (the listener).
- Jobs queued in JetStream at upgrade time are not migrated; the scheduler
  and the rescan task enqueue the periodic ones again.
- Event delivery to other nodes stays best effort; correctness relies on
  caches handling `EventResync` and on durable work going through the queue.
- Mixed-version clusters during a rolling upgrade do not share events or
  jobs (v0.1.9 nodes use NATS); upgrade all nodes together.
