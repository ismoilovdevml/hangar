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
| NATS | `nats.embedded: false`, `nats.url` pointing at a 3-node JetStream cluster (the Helm chart's `nats.mode: bundled`, or your own). |
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
- **Shutdown**: on SIGTERM a node fails `/readyz` first, so the load balancer
  stops sending new requests, and gives in-flight requests
  `server.shutdown_timeout` to finish. In Kubernetes the chart adds a
  pre-stop sleep so endpoints are removed before the signal.
- **Readiness**: `/readyz` fails when the node cannot reach PostgreSQL, the
  blob store or NATS. A database outage makes all nodes unready at once; the
  load balancer then answers 503 until it returns.

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

- **Job queue replication**: Hangar creates its JetStream job stream with one
  replica, and the replica count is not configurable yet. In a NATS cluster
  the stream lives on one NATS server; while that server is down, jobs cannot
  be queued and starting Hangar nodes restart until it is back. Events
  (publish/subscribe) are not affected. Keep NATS servers on separate hosts
  with persistent volumes and the PDB the chart creates.
- **Migrations take no lock**: run them once per upgrade (Helm hook Job, or
  `hangar migrate up` on one node), never with `migrate_on_start` on several
  nodes at the same time. The chart enforces this.
- **Node list**: `/api/v1/system/status` reports `nodes_up`/`nodes_total`
  from the node registry; with several replicas it may show fewer nodes than
  are running. Use the load balancer's or Kubernetes' view of healthy nodes.
- **Filesystem blobs on NFS**: every write is fsynced; NFS latency bounds
  publish throughput. Prefer S3 for several nodes.

## Multiple sites

Run one Hangar cluster per site against that site's database and blob
store. Replication between sites is on the roadmap (M6). Until then, use the
database's and the object store's own replication for disaster recovery, and
keep `HANGAR_SECRET_KEY` and `HANGAR_DOCKER_TOKEN_KEY` identical at the DR
site.
