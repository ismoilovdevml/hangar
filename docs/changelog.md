# Changelog

## Unreleased

- **Cleanup policies** (`/api/v1/cleanup-policies`): delete versions by age,
  download recency, keep-newest per package, version include/exclude regexes,
  keep globs, pre-release/snapshot only and minimum size, for attached
  repositories and repositories naming the policy in `config.cleanup_policies`.
  Quarantined versions and versions with the `legal_hold` attribute are never
  deleted. A dry-run preview (JSON or full CSV) is required before saving
  (`preview_token`); preview and run share one selection. Runs delete through
  the component service (audited, `component.deleted` events) in checkpointed
  batches, on a cron schedule or with Run now, and can be stopped.
- **Tasks** (`/api/v1/tasks`): built-in tasks (docker GC, blob compaction,
  auth purge, audit partitions) and the task of every cleanup policy, with
  editable cron schedules, Run now, Stop, run history, leveled run logs
  (`/tasks/{id}/runs/{run_id}/log`) and live `task.progress` /
  `task.finished` events. Runs execute on any node, survive node loss (taken
  over and resumed from their checkpoint) and are handed back on shutdown;
  only the scheduler leader starts scheduled runs.
- **Docker GC** (#7): deletes untagged manifests, their referrers entries and
  unreferenced layer/config blobs older than 7 days (configurable, dry run
  available); blob compaction then frees the bytes.
- **Blob stores**: `PUT` (soft quota, credentials; location only while
  empty) and `DELETE` (empty stores only) on `/api/v1/blob-stores/{name}`.
- **API** (#16): `GET /system/status` returns `base_url` and `registry_host`;
  `soft_quota_bytes` is nullable; `online` / `enabled` inputs are optional.
- Cleanup and task changes require `system:cleanup:write` /
  `system:tasks:write`; reads require `:read`.

## M0 — Skeleton (2026-11-03)

Foundation every later milestone builds on. No package format is functional yet.

- **Server**: single `hangar` binary with `serve`, `migrate` and `version`
  subcommands; chi router; request ids and JSON logs (`slog`); Prometheus
  `/metrics`; `/healthz` (liveness) and `/readyz` (PostgreSQL, blob store,
  NATS); graceful shutdown that fails readiness first and drains in-flight
  requests.
- **Configuration**: `hangar.yaml` plus `HANGAR_*` environment overrides, strict
  unknown-key checking and validation (`hangar.example.yaml`). Includes outbound
  HTTP proxy / CA / timeout settings for proxy repositories and trusted reverse
  proxies.
- **Core contracts** (`internal/core`): domain types (repositories, components,
  assets, digests, principals, privileges, events, jobs, audit events) and
  interfaces (`BlobStore`, `Format`, `Upstream`, `HTTPClientFactory`,
  `RepositoryService`, `ComponentService`, `Authenticator`, `TokenIssuer`,
  `AuthZ`, `EventBus`, `JobQueue`, `AuditLog`, `Deps`), with in-memory fakes and
  a blob store conformance suite in `internal/core/coretest`.
- **Formats**: registry and `/repository/{repo}/` dispatcher; `/v2/` root mount
  for OCI. Stub packages for docker, npm, maven and raw (answer 501).
- **Storage**: filesystem blob store with streaming sha256, dedup and atomic
  commit.
- **Database**: goose migrations for the full data model (blob stores, blobs,
  repositories, groups, components, assets, identity and access, cleanup
  policies, tasks, month-partitioned append-only audit log, nodes, replication,
  findings) with seeded built-in roles. Tested up/down against PostgreSQL.
- **Events**: embedded NATS JetStream for single-node mode or an external
  cluster; event bus and durable job queue with retries.
- **Outbound HTTP**: client factory honouring global/per-remote proxies
  (http, https, socks5), `no_proxy`, proxy environment variables, extra CAs,
  credential scoping and timeouts.
- **API**: `api/openapi.yaml` (OpenAPI 3.1) covering repositories, blob stores,
  components, search, findings, cleanup policies, tasks, users, roles, tokens,
  audit, cluster, overview metrics, system status and the SSE event stream.
  Implemented so far: `GET /api/v1/system/status`, `/system/version`, `/formats`;
  other endpoints answer 501.
- **Web UI hook**: `web/dist` is embedded with `-tags webui`; otherwise a
  fallback page is served.
- **Delivery**: Makefile, multi-stage Dockerfile (distroless, non-root),
  `deploy/docker-compose.yml` (PostgreSQL 16, MinIO, NATS, Trivy, Hangar), CI
  (vet/test with PostgreSQL, golangci-lint, govulncheck, web checks, image build).
- **Decisions**: ADRs 0001–0009 in `docs/adr/`.
