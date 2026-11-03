# Hangar — product specification

Every number shown in the UI comes from the API.

## Concepts

| Term | Meaning |
|---|---|
| Repository | Named container of one **format** and one **type**: `hosted`, `proxy` or `group`. |
| Hosted | Clients publish here. Write policy: `allow`, `allow_once` (immutable versions), `deny`. |
| Proxy | Fetches from a remote URL on demand and caches. Has metadata TTL, content TTL, negative-cache TTL, upstream auth, circuit breaker. |
| Group | Ordered list of member repositories of the same format behind one URL. Reads resolve in member order; writes go to a designated hosted member (Docker/npm). |
| Component | A package version (e.g. `payments-api:2.14.0`, `@acme/ui-kit@5.3.1`, `com.acme:ledger-core:7.0.0`). |
| Asset | A file belonging to a component (jar, pom, tgz, wheel, manifest, layer…). |
| Blob | Content-addressed bytes in a blob store; many assets can point at one blob. |
| Blob store | A storage backend (S3/MinIO bucket+prefix, or filesystem path) with soft quota. |
| Cleanup policy | Criteria that select components for deletion; attached to repositories; previewable. |
| Finding | A vulnerability or licence issue on a component, from the scanner. |
| Quarantine | A component blocked from download by policy until fixed or waived. |

## 01 · Sign in (`Login`)
- SSO button (OIDC, configured provider name), LDAP and SAML buttons, local username/password with "remember me".
- Hint for CLI/CI users: `hangar login --token`.
- Footer: version, docs and API links; header: system status dot.
- Lockout after N failed attempts (configurable), audit event on every attempt.

## 02 · Overview (`Main`)
- Greeting with date and one-line summary (packages published since yesterday, failed pushes).
- Range switch: 1h / Today / 7 days / 30 days — drives every widget below.
- KPI cards: storage used vs capacity, requests (+trend %, p95 latency), proxy cache hit ratio (+bytes saved), critical issues (+quarantined count).
- Traffic chart: downloads vs uploads per bucket.
- Storage by format: bar per format.
- Recently published: live list (SSE/WebSocket) with format badge, repo, publisher (user or CI identity), scan status, relative time. Row → package page.
- Upstream sources: health of each proxy remote (latency, healthy/slow/blocked).
- Connect a client: copyable snippets for the user's most-used formats, using the real base URL.

## 03 · Repositories
- Count cards for hosted / proxy / group.
- Tabs All/Hosted/Proxy/Group with counts, format filter, name filter, pagination.
- Table: name + description, type pill, format, URL, size, status (online, slow upstream, blocked by policy, offline), copy-URL button.
- "New repository" → screen 04. Row → repository settings (same form as 04, pre-filled, name/format read-only).

## 04 · New repository (`RepoCreate`)
- Step 1 format grid (Docker, npm, Maven, PyPI, Go, Helm, NuGet, Cargo, APT, Raw).
- Step 2 type cards with one-line explanations.
- Step 3 settings: name (validated `^[a-z0-9][a-z0-9-]{1,62}$`), blob store (shows free space), remote URL for proxy (prefilled per format), re-upload policy, cleanup policy, toggles: scan on arrival, quarantine critical, anonymous read. Group type shows a member picker with drag ordering.
- Summary panel with the final URL and the **same config as YAML** (and copyable `hangar` CLI / Terraform). Config-as-code must round-trip: `hangar apply -f repo.yaml` creates the identical repo.

## 05 · Package detail (`Browse`)
- Left tree: repository → package → versions.
- Header: format badge, name, version pill, signed badge, platform list, publisher and time; actions Download SBOM, Delete (RBAC + confirm).
- Pull/install command for the format, with copy.
- Tabs: Overview, Versions, Layers (OCI) / Files (others), Vulnerabilities, Dependents.
- Overview metadata: digest/checksums, size, downloads 30d, last pulled (+ by whom/which cluster), base image, source commit link.
- Versions list with scan status; Layers with size bars.

## 06 · Search
- Single search box with query language: free text, `format:`, `repo:`, `sha256:` prefix, `cve:`, `unused:>90d`, `license:`, `published:<2026-01-01`.
- Facets: format (with counts), security state, repository.
- Results: format badge, coordinates, repo, versions, downloads, security pill. Sort by downloads/newest.

## 07 · Blob stores (`BlobStores`)
- Totals: used vs capacity, saved by dedup, growth/day with time-to-full estimate, reclaimable now (+ run compaction).
- Card per store: type, location, state, used/capacity bar, soft quota, repositories count, blob count, write latency, encryption; actions Settings, Move repositories (online migration job).

## 08 · Cleanup policies (`Cleanup`)
- Policy list; editor written as readable sentences: delete versions older than N days AND not downloaded in N days; keep newest N per package; keep tags/versions matching glob list; optional pre-release/snapshot only.
- Applies-to repository chips.
- **Preview (dry run)** is mandatory before save: count, bytes, sample table, CSV export.
- Schedule shown and editable (cron + human text). Deleted items go to soft-delete; compaction frees space.

## 09 · Vulnerabilities (`Scanning`)
- Severity cards (critical/high/medium/low) with sub-notes.
- Findings table: advisory id + title, severity, package@version, fixed-in, affected components, status (open, assigned, waived until date, quarantined).
- Filters: fix available, in production (pulled in last N days), waived.
- Policy panel: quarantine critical, require signatures (cosign) per repo, licence deny-list.
- 30-day trend chart.
- Rescan on new advisory DB; waivers need reason + expiry, are audited.

## 10 · Access & audit (`Access`)
- Tabs: Members, Roles, Access tokens, Service accounts.
- Members table: avatar, name/email, role, source (OIDC/AD/local/token), tokens, last active.
- Sign-in providers status; audit log stream with export (CSV/JSON, also streamed to syslog/webhook).
- Roles = sets of privileges (`repo:<pattern>:read|write|delete|admin`, `system:*`); built-ins Admin, Publisher, Reader, CI deployer. Group→role mapping for OIDC/LDAP.
- Tokens: scoped to repos + actions, expiry required, shown once.

## 11 · Scheduled tasks (`Tasks`)
- "Running now" banner with progress and Stop.
- Table: task, description, schedule (human + cron), last run result with status dot, next run, Run now.
- Built-in task types: cleanup, blob compaction, docker GC, rescan, reindex search, directory sync, DB backup, replication, proxy metadata refresh.

## 12 · Cluster & replication (`Cluster`)
- Overall health + quorum.
- Node cards: role (leader/follower), zone, CPU/memory/cache-disk meters (warn colour > 75%), req/s, uptime.
- Replication targets (edge nodes, DR): repos replicated, lag or failure + retry, objects today; Add edge node.
- Dependencies health: PostgreSQL, object storage, event bus, search, scanner.

## Global
- Sidebar navigation, org switcher, system status card.
- ⌘K command palette: jump to package/repo/user/CVE, run actions.
- Notifications bell: quarantines, failed tasks, quota warnings.
- Every list is URL-addressable (filters in query string). Every page works at 390 px width.
