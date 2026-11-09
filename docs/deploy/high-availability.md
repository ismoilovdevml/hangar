# High availability

Hangar nodes are stateless: any node serves any request, and no sticky
sessions are needed. Availability comes from running two or more nodes on
shared, highly available dependencies.

```
              load balancer / ingress  (health check: /readyz)
               |            |            |
           hangar-1     hangar-2     hangar-3        same config and keys
               \            |            /
      PostgreSQL (HA)   S3 / shared FS   NATS JetStream cluster (3)
```

## What each node needs

| Shared | Requirement |
|---|---|
| PostgreSQL | One database for all nodes, made highly available by the platform: a managed service, Patroni, CloudNativePG. Connections: `nodes x (database.max_conns + 16)`. |
| Blob store | `storage.kind: s3` (recommended) or one filesystem mounted at the same path on every node (NFS, CephFS, a ReadWriteMany volume). |
| NATS | `nats.embedded: false`, `nats.url` pointing at a 3-node JetStream cluster (the Helm chart's `nats.mode: bundled`, or your own), and `nats.replicas: 3` (see [Job queue replicas](#job-queue-replicas)). |
| `HANGAR_SECRET_KEY` | Identical on every node; otherwise a node cannot read credentials another node stored. |
| `HANGAR_DOCKER_TOKEN_KEY` | Identical on every node; otherwise a registry token or an upload session started on one node is rejected by the next. |
| Configuration | Identical `hangar.yaml` (the Helm chart renders one ConfigMap). |

## How the cluster behaves

- **Requests**: every request is independent. Docker chunked uploads carry
  their state in signed upload URLs, so consecutive chunks may hit different
  nodes. Sessions are stored in PostgreSQL.
- **Background jobs**: every node runs a scheduler, and one is the leader
  (PostgreSQL advisory lock). The leader enqueues the periodic jobs (blob GC,
  audit partitions, cleanup, token purge); any node executes them from the
  NATS job queue. When the leader dies, its lock is released and another node
  takes over within seconds.
- **Caches**: repository lookups, authorization decisions and generated
  metadata are cached per node and invalidated through NATS events. A node
  that misses an event (NATS disconnect at that moment) can serve a stale
  repository setting for up to 30 s, authorization for up to
  `auth.authz_cache_ttl` (5 s), and generated Maven metadata for up to a
  minute.
- **Shutdown**: on SIGTERM a node fails `/readyz` at once, keeps serving new
  requests for `server.drain_delay` (default 0) so a load balancer that polls
  `/readyz` takes it out of rotation, then stops listening and gives in-flight
  requests `server.shutdown_timeout` to finish. See
  [Draining nodes](#draining-nodes).
- **Migrations**: `hangar migrate up` and `migrate_on_start` hold a
  PostgreSQL advisory lock while they run. Nodes that start together wait
  for the one that migrates and then start on the new schema; a node without
  `migrate_on_start` waits too instead of failing on a half-migrated schema.
- **Readiness**: `/readyz` fails when the node cannot reach PostgreSQL, the
  blob store or NATS. A database outage makes all nodes unready at once; the
  load balancer then answers 503 until it returns.

## Job queue replicas

Background jobs are queued in one JetStream stream, `HANGAR_JOBS`.
`nats.replicas` (1 to 5, default 1) sets its replica count; the job
consumers follow the stream. With 1 the stream lives on a single NATS
server: while that server is down jobs cannot be queued and starting nodes
exit with `create job stream: ... stream is offline`. With 3 on a 3-node
cluster the stream keeps working while any one NATS server is down.

```yaml
nats:
  embedded: false
  url: nats://hangar:password@nats-1:4222,nats://hangar:password@nats-2:4222,nats://hangar:password@nats-3:4222
  replicas: 3
```

Every node creates or updates the stream on start, so set the same value on
all nodes. Changing it scales the existing stream in place when the next node
starts (logged as `job stream replicas changed`); queued jobs are kept. The
value cannot exceed the number of NATS servers (the start fails with
`update job stream HANGAR_JOBS (replicas 3 to 5): ... no suitable peers for
placement` and the stream keeps its replicas), and embedded NATS accepts
only 1. Check the
result with `nats stream info HANGAR_JOBS` (Replicas, and the cluster
members listed as current).

The Helm chart sets it from `nats.streamReplicas`: `0` (default) uses
`nats.bundled.replicas` with the bundled cluster (3) and 1 otherwise. Upgrading
a release with the bundled cluster therefore scales the job stream from 1 to
3 replicas.

## Draining nodes

Stopping a node without dropping requests needs the load balancer to stop
sending it traffic before its listener closes, and the service manager to
wait for in-flight uploads:

| Setting | Meaning |
|---|---|
| `server.drain_delay` (0s to 5m, default 0s) | After SIGTERM, `/readyz` answers 503 while the node keeps serving new requests (with `Connection: close`) for this long. Set it to the time your load balancer needs to notice: health check interval x unhealthy threshold, plus one interval. |
| `server.shutdown_timeout` (1s to 24h, default 30s) | After the drain delay, the listener closes and in-flight requests get this long to finish; then they are cut off. |
| Service manager timeout | Must exceed `drain_delay + shutdown_timeout`: systemd `TimeoutStopSec`, Docker `stop_grace_period`, Kubernetes `terminationGracePeriodSeconds`. |

In Kubernetes, endpoints are removed when the pod starts terminating,
independent of readiness. The chart's `lifecycle.preStopSleepSeconds` (5)
delays SIGTERM until kube-proxy and ingress controllers have caught up, so
`drain_delay` can stay 0. Keep

```
terminationGracePeriodSeconds > preStopSleepSeconds + drain_delay + shutdown_timeout
```

(chart defaults: 60 > 5 + 0 + 30). Set `config.server.drain_delay` instead of
the sleep on clusters older than 1.30 (no `sleep` pre-stop action), and in
addition to it when a load balancer outside the cluster health-checks the
pods directly (for example `drain_delay: 15s` for a 5 s check interval and a
threshold of 2).

## Failure scenarios

Observed on a test cluster (Helm chart, 2 Hangar replicas, 3 NATS nodes,
MinIO, PostgreSQL):

| Failure | Effect |
|---|---|
| Registry token issued by one replica, used on the other | Accepted (shared `HANGAR_DOCKER_TOKEN_KEY`). |
| `helm upgrade` with a configuration change | Migration Job ran, pods were replaced one at a time, generated keys stayed the same, previously uploaded artifacts and images were served by the new pods. |
| Two of three NATS nodes restarted at once | One `/readyz` probe answered 503 (JetStream lost quorum), recovered within seconds once the nodes were back. |
| NATS job stream unavailable while a Hangar pod starts | The pod exits with `create job stream: ... stream is offline` and is restarted by Kubernetes until the stream is available (see limits). |

## Known limits

- **Job queue replication** needs `nats.replicas` above 1 (see
  [Job queue replicas](#job-queue-replicas)); with the default of 1, a NATS
  server outage stops job queueing until it is back. Events
  (publish/subscribe) are not replicated state and are not affected. Keep
  NATS servers on separate hosts with persistent volumes and the PDB the
  chart creates.
- **Migration lock**: the advisory lock needs a session-level connection.
  Run `hangar migrate up` and nodes with `migrate_on_start` directly against
  PostgreSQL or through a session-pooling proxy, not PgBouncer in
  transaction pooling mode. A node waiting for the lock waits as long as the
  migration takes.
- **Node list**: `/api/v1/system/status` reports `nodes_up`/`nodes_total`
  from the node heartbeats in PostgreSQL (every 15 s); a node that stopped is
  counted as down after 45 s and forgotten after an hour.
- **Filesystem blobs on NFS**: every write is fsynced; NFS latency bounds
  publish throughput. Prefer S3 for several nodes.

## Multiple sites

Run one Hangar cluster per site against that site's database and blob
store. Replication between sites is on the roadmap (M6). Until then, use the
database's and the object store's own replication for disaster recovery, and
keep `HANGAR_SECRET_KEY` and `HANGAR_DOCKER_TOKEN_KEY` identical at the DR
site.
