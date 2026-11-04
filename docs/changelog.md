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
