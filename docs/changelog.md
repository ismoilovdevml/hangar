# Changelog

## Unreleased

- **Secrets at rest**: proxy upstream passwords, tokens and header values,
  passwords in proxy remote and outbound proxy URLs, and S3 keys of blob
  stores are encrypted in the database (AES-256-GCM envelope encryption,
  key from `HANGAR_SECRET_KEY`). Existing plaintext values are sealed on the
  first start with a key; the database rejects plaintext secrets. Without a
  key, configurations carrying credentials are refused (`501`).
- **Key rotation**: `security.previous_secret_keys` and
  `hangar admin rotate-secrets [--dry-run]`.
- **API**: every route except `GET /system/status`, `GET /system/version`,
  `GET /auth/providers` and `POST /auth/login` now requires sign-in
  (anonymous callers previously could list repositories, components and
  search results of anonymously readable repositories).
- **Proxy repositories**: remote URLs pointing at loopback, private,
  link-local or other internal addresses are refused when the repository
  is saved (`400`), not only when fetching.
- **HTTP hardening**: security headers and content security policies for
  the UI, API and package endpoints; HSTS on HTTPS; CORS denied by default
  (`security.cors.allowed_origins`); API body limit (1 MiB), streamed
  uploads, body idle timeout and header size limit; per-IP rate limits for
  the API and for failed authentications (`security.rate_limit`).
- **Security tests**: `test/security` sweeps every API operation and package
  endpoint for authorization, SSRF and secret handling against a running
  server.
- **Docs**: `docs/security.md` (threat model, secret storage, operator
  checklist) and `SECURITY.md` (vulnerability reporting).
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
- **Web UI · Cleanup policies**: policy list and an editor written as sentences
  (age, last download, pre-release only, keep newest N, keep tags matching
  globs), repository chips, a schedule builder that shows the cron in plain
  words with the next run, a mandatory preview (count, bytes, sample, full CSV
  export) before saving, Run now and Delete with confirmation, and run history.
- **Web UI · Scheduled tasks**: "Running now" banner with live progress (SSE
  `task.progress`), time-left estimate and Stop; task table with schedule, last
  and next run, type/status/name filters in the URL, and Run now (confirmation
  for tasks that delete data). Task page with paged run history and a run view
  with progress and a live log.
- **Web UI · Audit log**: filters for actor, action, target, outcome and time
  range (presets or a custom UTC range) kept in the URL, expandable event
  details, cursor paging, and CSV / NDJSON export with the same filters.
- **Web UI · System**: status, version and build, uptime, nodes, dependency
  health, base URL and registry host, and the effective configuration with
  secrets masked.
- Run, stop, save and delete actions are hidden from users without the matching
  `system:<area>` privilege; screens say plainly when a server does not support
  an endpoint yet.
- **Observability**: Prometheus metrics for package traffic per format and
  repository type (requests, latency, upload and download bytes), blob store
  operations per driver, store usage, quotas and filesystem free space, proxy
  remote fetches and circuit breaker state, database statements and pool,
  job queue depth and job outcomes, NATS connection, authentication failures
  and lockouts, the audit queue and readiness. Labels are bounded: route
  patterns, standard methods and capped value sets, never raw paths.
- **Tracing**: OpenTelemetry traces over OTLP/HTTP
  (`observability.tracing.{enabled,endpoint,sample_ratio,service_name}`) with
  spans for HTTP requests, database statements, blob store operations, proxy
  fetches and jobs; incoming W3C `traceparent` headers are honoured.
- **Logs**: every request log record carries `request_id`, `trace_id`,
  `principal`, `repository` and `format`; credentials are scrubbed from all
  log output; `observability.access_log.sample_ratio` samples the access log
  (server errors are always logged). The access log fields `bytes` became
  `bytes_in` and `bytes_out`.
- **Deploy**: Grafana dashboards (overview, formats, storage, upstreams, jobs,
  auth) and Prometheus alert rules with unit tests in `deploy/observability/`;
  metric reference in `docs/operations/monitoring.md` and a runbook per alert
  in `docs/operations/runbooks.md`.
- **Fix**: request metrics (and the Overview screen numbers) are recorded when
  `metrics.enabled` is false; only the `/metrics` endpoint is turned off.
- **Proxy related hosts**: proxy repositories fetch resources their remote
  links to on other hosts through the same upstream engine (SSRF guard,
  per-host circuit breaker, request collapsing, negative cache, outbound
  proxy, metrics), never sending the remote's credentials there. PyPI file
  downloads (files.pythonhosted.org) and NuGet search/registration hosts named
  by the service index (azuresearch-*.nuget.org) use it; npm follows tarball
  CDNs listed in the new `proxy.related_hosts`.
- **Rate limits (429)**: a remote's `Retry-After` is honoured for retries
  within a 10s budget; beyond that clients get 429 with the remote's
  `Retry-After` instead of 502 (a cached copy is served when there is one),
  the host is held until the wait passes and 429s no longer trip the circuit
  breaker. A rate-limited remote reports `slow` on the upstream health page.
- **Upstream tuning** per proxy repository: `max_concurrent_requests`,
  `http.max_conns_per_host`, `http.max_idle_conns_per_host`,
  `http.disable_http2`, and `http.body_timeout` (stall timeout between body
  reads; 60s default). HTTP/2 is negotiated with health pings; interrupted
  large downloads resume with Range/If-Range when the remote supports it.
- **Offline mode** (`proxy.offline`): a proxy repository serves only its
  cache and answers 404 for anything not cached, without contacting the remote.
- **Cache purge**: `POST /api/v1/repositories/{name}/cache:invalidate` forgets
  a proxy repository's remembered 404s and rate-limit holds on every node
  (audited).
- **Metrics**: `hangar_proxy_upstream_requests_total{repository,host,result}`,
  `hangar_proxy_upstream_request_duration_seconds`,
  `hangar_proxy_upstream_retries_total`, `hangar_proxy_upstream_resumes_total`,
  `hangar_proxy_upstream_inflight_requests`.
- **Access management** (see `docs/guides/access.md`):
  - Members and service accounts: search and filter by role, status
    (active, locked, disabled) and source; disable/enable, unlock, admin
    password reset, sign out everywhere, delete — with guards for your own
    account and the last administrator. Service accounts take a description.
  - Self-service: `PUT /me/password` (current password required, other
    sessions end), `PUT /me/profile`, `GET/DELETE /me/sessions`.
  - Personal access tokens: last use is recorded with the client address;
    administrators list everyone's tokens (`GET /tokens?all=true` with owner,
    owner kind, status and name filters); scopes are validated; the session
    payload carries the token policy and expiry warnings
    (`auth.token_expiry_warning`, default 14 days).
  - Roles: `GET /roles/{name}`, add/remove members
    (`/roles/{name}/members`), privilege validation.
  - Effective permissions report: `GET /users/{id}/permissions` lists, per
    repository and system area, the allowed actions and the role, privilege
    or anonymous read behind each.
  - Every change and every refused attempt is audited (`user.disable`,
    `user.enable`, `session.revoke`, `session.revoke_all`, `role.bind`,
    `role.unbind`, …).
  - Web UI: Access & audit (members, service accounts, access tokens, roles
    with a privilege editor, access audit log) and My account (profile,
    password, tokens, signed-in devices, own permissions), with a
    create-token dialog that builds scopes, offers expiry presets and shows
    the secret once.
  - Migration `00040`: `tokens.last_used_ip`, `users.description`, token
    indexes.
- **Docker / OCI — push through a group fixed**: a group with a Docker Hub
  proxy member answered `502` to the existence probe of a new tag
  (`HEAD /v2/<group>/<image>/manifests/<tag>`), because Docker Hub answers
  `401` for unknown repositories. Remote `401/403/404/400 NAME_INVALID` now
  mean `MANIFEST_UNKNOWN` / `BLOB_UNKNOWN`, `429` is passed on as
  `TOOMANYREQUESTS`, and a group treats an unreachable remote as a miss.
- **Docker / OCI — proxy blob dedup is confirmed with the remote**: a blob
  already in the shared store is linked into a proxy only after a `HEAD`
  confirms the remote serves it under that name (no more serving stored
  bytes by digest alone).
- **Docker / OCI — quarantine by digest**: manifests pulled by digest,
  platform manifests of a quarantined index, blobs that belong only to
  quarantined images and cross-repository mounts are refused (`403 DENIED`).
- **Docker / OCI — connectors**: `connector.host` (e.g. `cr.example.com`)
  and `connector.http_port` route a host name or a dedicated port to one
  docker repository, with image names without the repository prefix and
  tokens bound to that entry point. nginx and Caddy examples in
  `internal/formats/docker/README.md`.
- **Docker / OCI — upload sessions**: session records live in the blob
  store temp area (new optional `core.TempStore`, implemented by the fs and
  S3 drivers) instead of leaking a blob per request; sessions resume on any
  node and across restarts; the hourly built-in task `docker-upload-cleanup`
  ends abandoned sessions and releases leftover chunks to blob compaction.
- **npm**: unpublished versions leave a tombstone and cannot be published
  again (`403`), as on the public registry; set
  `config.format.reuse_unpublished_versions: true` to allow reuse.
  Unpublishing now also needs `repo:<name>:write` (it rewrites the package
  state).
- **npm**: package state changes (publish, dist-tags, deprecate, unpublish)
  are serialised per package across all nodes with PostgreSQL advisory locks
  (a dedicated pool of up to 16 connections per node), so concurrent dist-tag
  updates on different nodes are no longer lost.
- **Performance**: hosted npm packuments are rendered once per change (about
  6x the throughput of the previous build; 100-version packument p95 from
  99 ms to 4 ms); generated Maven metadata is cached with event invalidation
  (document and checksums always agree); repository lookups are cached;
  component and asset listings keep using their indexes under generic query
  plans. Results: `docs/operations/performance.md`.
- **NuGet**: hosted search uses a PostgreSQL full-text index (migration
  `00070`). Words now match as prefixes of tokens: `json` and `newt` find
  `Newtonsoft.Json`, a substring such as `tonsoft` no longer does.
- **Events**: `asset.published` and `asset.deleted` for assets without a
  component (repository metadata, raw files).
- **Tools**: `cmd/hangar-seed` fills a database with synthetic components and
  assets; k6 load scenarios in `test/load`.
- **Docs**: user guides in `docs/guides` (getting started, one page per
  client, CI integration, REST API) and a shorter README.

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
