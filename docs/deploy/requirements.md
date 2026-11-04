# Requirements and sizing

## Components

| Component | Required | Notes |
|---|---|---|
| Hangar | yes | One static binary (`linux/amd64`, `linux/arm64`) or the container image `ghcr.io/ismoilovdevml/hangar` (distroless, runs as UID 65532). Stateless apart from the embedded NATS and filesystem blob options. |
| PostgreSQL | yes | Version 16 or newer. Holds repositories, components, assets, users, tokens, audit log, task state. Hangar applies its own schema migrations. |
| Blob storage | yes | **S3-compatible** (AWS S3, MinIO, Ceph RGW, ...) or a **filesystem path**. Blobs are content-addressed and immutable. Several nodes need S3 or a shared ReadWriteMany volume. |
| NATS with JetStream | yes | Events and the job queue. Embedded in the process for a single node; an external cluster (3 nodes) for several nodes. |
| Reverse proxy / ingress | for TLS | Hangar serves plain HTTP. Terminate TLS in Caddy, Traefik, a cloud load balancer or an ingress controller. |
| OSV API (`api.osv.dev:443`) or a mirror | no | Vulnerability scanning of npm, Maven, PyPI, NuGet and Go packages (`scanning.osv.url`); without it scans fail and are retried. |
| Trivy server | no | `scanner.trivy_url` is reserved for container image scanning, which is not active yet. |

PostgreSQL user rights: Hangar needs to own its schema (create tables,
indexes, partitions and run migrations). Create a dedicated database and
role:

```sql
CREATE ROLE hangar LOGIN PASSWORD '...';
CREATE DATABASE hangar OWNER hangar;
```

S3 rights for the bucket: `s3:ListBucket`, `s3:GetBucketLocation`,
`s3:ListBucketMultipartUploads` on the bucket, and `s3:GetObject`,
`s3:PutObject`, `s3:DeleteObject`, `s3:AbortMultipartUpload`,
`s3:ListMultipartUploadParts` on its objects. The compose stack creates a
MinIO user with exactly this policy.

## Sizing

The numbers below are starting points for planning, derived from the single-
node measurements in [performance.md](../operations/performance.md) (one node
served 14 000 to 28 000 metadata and download requests per second on a
12-core machine with the database on the same host) and from the memory
settings Hangar uses. Measure your own workload before committing hardware;
the Grafana dashboards in `deploy/observability/` show request rates,
latencies and resource use per node.

| Size | Users / CI runners | Hangar | PostgreSQL | Blob storage |
|---|---|---|---|---|
| Small | up to 50 / 10 | 1 node: 2 vCPU, 4 GiB | 2 vCPU, 4 GiB, 20 GiB SSD | 200 GiB |
| Medium | up to 500 / 100 | 2 nodes: 4 vCPU, 8 GiB each | 4 vCPU, 16 GiB, 100 GiB SSD | 1-5 TiB S3 |
| Large | 500+ / 100+ | 3+ nodes: 8 vCPU, 16 GiB each | 8+ vCPU, 32 GiB, replicated | S3, sized from growth |

What drives each resource:

- **CPU**: request rate, TLS (at the proxy), SHA-256 of every upload, argon2
  password checks. Clients that authenticate with tokens are much cheaper
  than password logins.
- **Memory** per node: Go heap for in-flight requests and caches (npm
  packuments, Maven metadata, repository lookups), plus argon2:
  `auth.argon2.memory_kib` (64 MiB) x `auth.max_concurrent_hashes` (4) =
  256 MiB at peak. Uploads and downloads are streamed and do not grow with
  artifact size. Start with a 2 GiB limit per node and watch
  `go_memstats_heap_inuse_bytes`.
- **Database connections**: each node uses up to `database.max_conns` (20)
  plus up to 16 connections for advisory locks. Set PostgreSQL
  `max_connections` to at least `nodes x (max_conns + 16)` plus room for
  backups and administration.
- **Database size**: metadata only (no artifact bytes), roughly a few KiB per
  asset plus the audit log, which is partitioned by month.
- **Blob storage**: the sum of unique artifact bytes (identical layers and
  files are stored once) plus cached proxy content. Cleanup policies and blob
  GC reclaim space; GC keeps unreferenced blobs for
  `storage.gc.grace_period` (48h) before deleting them.
- **Disk for filesystem blobs**: local SSD or NFS. Each write is fsynced, so
  flush latency bounds publish throughput.
- **Embedded NATS**: a few hundred MiB of disk under `nats.data_dir`.

## Network

| From | To | Port | Purpose |
|---|---|---|---|
| Clients, CI | reverse proxy | 443 | UI, API, every package format |
| Reverse proxy | Hangar | 8080 (`server.addr`) | HTTP |
| Docker clients | Hangar | per connector | only for Docker port connectors (`connector.http_port`) |
| Hangar | PostgreSQL | 5432 | |
| Hangar | S3 endpoint | 443 / 9000 | blob storage |
| Hangar | NATS | 4222 | external NATS only |
| Hangar | Internet or corporate proxy | 443 | proxy repositories (Maven Central, npmjs, Docker Hub, PyPI, nuget.org) |
| Prometheus | Hangar | 8080 `/metrics` | scraping; keep `/metrics` off public hosts |

Docker clients need the registry at the root of a host name (`/v2/`). Hangar
serves it on the main host, and the deploy examples add an optional separate
registry host (`registry.example.com`), see [reverse-proxy.md](reverse-proxy.md).

## Large artifacts

Hangar streams request and response bodies and has no upload size limit by
default (`security.limits.upload_body_bytes: 0`). Everything in front of it
must allow the same:

- no request body size limit,
- no request buffering (spooling multi-GiB uploads to the proxy's disk),
- read and write timeouts measured between two reads, not for the whole
  transfer, of at least several minutes.

[reverse-proxy.md](reverse-proxy.md) has the exact settings for Caddy,
Traefik and the Kubernetes ingress controllers.
