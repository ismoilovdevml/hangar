# Hangar — architecture

## Runtime topology

```
                  ┌──────────── clients ─────────────┐
                  docker  npm  mvn  pip  go  helm  CI
                              │ HTTPS
                     ┌────────▼────────┐   (L7 load balancer, sticky not required)
                     │  hangar node ×N │  single Go binary: format handlers, API, UI, jobs
                     └───┬────┬────┬───┘
         PostgreSQL ◄────┘    │    └────► S3 / MinIO / FS  (blobs, content-addressed)
         (metadata,           │
          audit, jobs)   NATS JetStream (events, job queue, replication stream)
                              │
                 Trivy server · OpenSearch (optional) · OIDC/LDAP
```

- Nodes are stateless apart from a local read-through blob cache (LRU on disk).
- Leader (via PostgreSQL advisory lock) runs schedulers; any node executes queued jobs.
- Single-node mode: embedded NATS, filesystem blobs, one PostgreSQL — `docker compose up` works.

## Request path (download)
1. Router matches format mount (`/v2/` for OCI, `/repository/<repo>/...` for others, plus vanity hosts like `cr.acme.io`).
2. Auth: bearer/basic/token → principal; anonymous if repo allows.
3. RBAC check `repo:<name>:read`.
4. Format handler resolves path → component/asset (group: walk members in order; proxy: cache lookup → upstream fetch on miss with singleflight).
5. Quarantine check → 403 with explanation body.
6. Stream blob from local cache or blob store; record download (batched, async) for "last downloaded" and stats.

## Upload path
Handler validates format rules → streams to temp blob while hashing → dedup by sha256 → transaction: component/asset rows, write policy check → emits `component.published` → scanner job, search indexer, replication, live feed.

## Core interfaces (Go, `internal/core`)

```go
type BlobStore interface {
    Put(ctx context.Context, r io.Reader) (Digest, int64, error) // streams, hashes, dedups
    Get(ctx context.Context, d Digest) (io.ReadSeekCloser, error)
    Stat(ctx context.Context, d Digest) (BlobInfo, error)
    Delete(ctx context.Context, d Digest) error // only by GC/compaction
}

type Format interface {
    Name() string                          // "docker", "npm", ...
    Mount(r chi.Router, deps Deps)         // registers protocol routes
    Validate(repo *Repository) error       // format-specific config checks
    Coordinates(asset *Asset) Coordinates  // for UI/search
}

type Upstream interface { // proxy repos
    Fetch(ctx context.Context, path string, cond Conditional) (*UpstreamResponse, error)
}
```

## Data model (PostgreSQL, simplified)

```
blob_stores(id, name, kind, config jsonb, soft_quota_bytes)
blobs(digest pk, store_id, size, created_at, ref_count, soft_deleted_at)
repositories(id, name unique, format, type, blob_store_id, config jsonb, online)
group_members(group_id, member_id, position)
components(id, repo_id, namespace, name, version, created_at, published_by,
           last_downloaded_at, download_count, quarantined bool, attributes jsonb,
           unique(repo_id, namespace, name, version))
assets(id, component_id null, repo_id, path, blob_digest, content_type, size,
       checksums jsonb, created_at, unique(repo_id, path))
findings(id, component_id, advisory_id, severity, cvss, package, installed, fixed_in,
         status, waiver_reason, waiver_until, assignee)
users, roles, privileges, role_bindings, tokens(hash, scopes, expires_at), identity_providers
cleanup_policies(id, name, criteria jsonb, schedule), policy_repos
tasks(id, type, schedule, config, enabled), task_runs(id, task_id, status, progress, log, started, finished)
audit_events(id, at, actor, action, target, ip, details jsonb)  -- append-only, partitioned by month
nodes(id, address, role, last_heartbeat, stats jsonb), replication_targets, replication_state
```

## API

- `GET/POST /api/v1/...` JSON REST for UI and automation, described in `api/openapi.yaml`.
  Resources: repositories, blob-stores, components, assets, search, findings, waivers,
  cleanup-policies (+ `:preview`), tasks (+ `:run`, `:stop`), users, roles, tokens,
  audit, nodes, replication, system/status, metrics/overview.
- `GET /api/v1/events` — Server-Sent Events: publishes, quarantines, task progress.
- Config as code: `hangar apply -f` / `hangar export` (YAML), Terraform provider later.
- Health: `/healthz` (liveness), `/readyz` (DB, blob store, NATS reachable), `/metrics`.

## Security
- TLS everywhere; HSTS on UI.
- Passwords argon2id; tokens stored as SHA-256 hashes, prefix `hgr_` for secret scanners.
- CSRF protection for cookie sessions; tokens for CLI/CI.
- Rate limiting per principal and per IP; lockout on failed logins.
- Proxy SSRF guard: remote URLs must match an allow-list; block private ranges unless allowed.
- Cosign signature verification for OCI where policy requires.

## Integrations (no hard dependency)
- CI systems (GitLab CI, Gitea Actions, GitHub Actions, Jenkins, Drone) authenticate with service-account tokens; docs show snippets for each.
- Optional OIDC token exchange so CI jobs use their own JWT (GitLab `id_tokens`, GitHub OIDC) instead of long-lived secrets.
- Webhooks on events; Slack/Teams via webhook.
