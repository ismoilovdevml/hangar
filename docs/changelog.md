# Changelog

## Unreleased

- **Settings hub**: a new Settings page (`/settings`, avatar menu → Settings,
  ⌘K) gathers addresses, instance name, sign-in, internet access, storage,
  nodes, configuration, system status, system logs, the audit log, My account
  and access tokens in grouped rows, each with a one-line live status
  ("corp-proxy · proxy.example.com:3128", "All 14 checks passing"). Rows follow
  the user's privileges: a user without system privileges sees only their
  account. The System page moved from `/settings` to `/settings/system`; every
  other address (`/account`, `/audit`, `/system/status`, `/system/logs`,
  `/settings/authentication`, `/settings/proxies`) is unchanged and shows
  "Settings" in its breadcrumb.
- **Shorter sidebar**: System status, System logs, System, Audit log, My
  account and the Settings section left the sidebar. A page without its own
  entry lights up the entry of its area (Access & audit or Cluster &
  replication). The command palette still finds every page and hides the ones
  the user may not open.
- **README images**: badges, logos and screenshots in package READMEs now
  load. The package info API returns, for every image of a README, a
  same-origin URL signed by the server (`readme.images`); Hangar fetches
  remote images through the outbound proxy and address policy (https only,
  5 MiB, images only, content sniffed, SVG sandboxed) and caches them for
  24 hours in memory and PostgreSQL (`readme_image_cache`, migration
  `00160`). New endpoint `GET /components/{id}/readme-images/{sig}`. Relative
  image paths are served from the package (.nupkg, npm tarball) or resolved
  in the source repository on github.com / gitlab.com; image links copied
  from github.com pages are unwrapped. New setting `ui.readme_images`
  (`proxy` default, `direct`, `off`) and `ui.readme_images_allow_http`; see
  [security](security.md#readme-images) for the privacy trade-off.
- **README rendering**: badge rows stay on one line, linked images stay
  clickable, `<img width>` is honoured, images are bounded and load lazily
  without a referrer, and an image that is not shown or fails to load becomes
  a small chip with its alternative text instead of `[image: …]`.
- **Package icon**: NuGet packages show their own icon (`<icon>` in the
  .nupkg, or `<iconUrl>` through the image proxy) in the package header
  (`icon_url` in the package info API).
- **Overview, rebuilt**: a search hero with quick actions (connect a client,
  create a token, new repository, browse packages); an activity sentence with
  requests per interval; a request outcome ring (share answered without a 5xx
  or a 401/403, p95); a To do list built from real signals (unhealthy system or
  upstreams, rejected pushes, critical findings, expiring tokens, missing or
  failed database backup, scanning off, no storage quota); Storage, Security
  and Upstream cards; the most downloaded packages; recently published.
  `GET /metrics/overview` adds `activity` (published, cached, pushes rejected
  over the range), `requests.server_errors`, `requests.denied` and per-bucket
  `traffic[].requests`; the outcomes are recorded from this version on
  (migration `00170`).
- **Account menu**: the avatar opens a solid menu with name, email and role,
  My account, Access tokens, Password & sessions (Signed-in devices for single
  sign-on users) and Sign out, with full keyboard support.
- **Sidebar status card** stays pinned at the bottom while the links scroll,
  links to System status and turns amber or red when the system is degraded or
  down. The bell shows a dot while tokens are about to expire.
- **Settings → Authentication, presets first**: pick the identity provider
  (Active Directory, OpenLDAP, FreeIPA, Keycloak, Microsoft Entra ID, Google
  Workspace, Okta or any other OIDC / LDAP) and fill in three fields; the
  preset sets scopes, claims, filters, attributes and nested groups, with
  every field one *Customize* away. Setup steps per provider next to the
  redirect URI, status chips for the methods in use, *Switch to …* when
  another provider replaces the one in use. Typed credentials survive
  switching cards. The chosen preset is stored as `preset` on
  `/identity-providers/oidc` and `/identity-providers/ldap` (optional,
  informational).
- **Settings → Proxies, calm and short**: with no proxy the page says how
  remotes are reached (directly, or through the server's `HTTPS_PROXY`).
  A new proxy needs one field, the proxy address as in `HTTPS_PROXY`, plus
  the platform-default switch; credentials, exceptions and the CA
  certificate (upload or paste) are folded away. The test reports the
  answer inline. Proxies are listed with their default flag and usage.
- **Vulnerability fix plan**: the Vulnerabilities screen groups findings by
  package and says what to upgrade: the vulnerable versions present, the
  lowest version fixing every finding per release line (e.g.
  `8.0.4 / 10.0.10`), a copyable `dotnet add package` / `npm install` /
  `pip install` / `mvn versions:use-dep-version` / `go get` command, and how
  often the vulnerable versions were pulled in the last 7 days and by how
  many clients. Packages nobody downloaded within `scanning.in_use_window`
  (default 30 days) are dimmed and listed last; a banner sums up what the
  upgrades fix. Tabs *In use first*, *All packages* and *Waived*, CSV export
  (`GET /vulnerabilities/fix-plan?format=csv`).
- **Who pulls a package**: downloads are attributed per package version, UTC
  day and client (principal, or address and client kind) in
  `component_pulls` (migration `00150`, kept 90 days), batched like the
  download counters and summed over every node. The finding drawer lists the
  busiest clients of the last 7 days (`GET /findings/{id}/pullers`,
  `system:security:read`).
- **Assign findings** to a Hangar user, one finding or a whole package (new
  findings of the package follow), shown in the fix plan with an *Assigned to
  me* filter; audited as `finding.assign` / `finding.unassign`. Waive a whole
  package or one of its findings in one step.
- **Rescan** on the Vulnerabilities screen (`system:security:write`) queues a
  scan of every repository and shows its progress
  (`POST/GET /vulnerabilities/rescan`, audited as `vulnerability.rescan`).
- The trend chart, the scanner panel and the per-repository *Scan now*
  button left the Vulnerabilities screen; `GET /findings/trend` and
  `POST /repositories/{name}:scan` are unchanged.

- **Fewer release files**: the compose stack ships as one `hangar-compose.tar.gz`
  and only Linux binaries are published (`hangar-linux-amd64`,
  `hangar-linux-arm64`), next to `install.sh` and `SHA256SUMS`. The installer
  still reads releases that publish each compose file separately.
- **System status** (Operations → System status, `system:cluster:read`): every
  node with its URL, version and commit, uptime, last heartbeat and scheduler
  role, and the health of PostgreSQL, blob stores, NATS / JetStream, the task
  scheduler, the search index, external sign-in and upstream proxies, each
  healthy, degraded or down with details. Nodes publish their checks every
  15 s into `system_nodes` (migration `00140`); a silent node shows as not
  responding. New endpoints `GET /system/nodes` and `GET /system/health`;
  `GET /nodes` is implemented from the same heartbeats and
  `/system/status` reports real `nodes_up` / `nodes_total`.
- **System status redesign**: the page leads with one headline ("Everything
  is working", "1 check needs attention") and the share of checks passing,
  then a checklist per node: one row per subsystem with its summary and a
  headline value (ping, free space, scheduler role, probe time, slowest proxy
  latency). Rows with a problem open by themselves; an open row shows the
  details, what the check probes, since when the status holds, the last
  incident and a link to the matching page. Live refresh pauses from the
  "Live · every 15 s" toggle. `SystemSubsystem` gains `metric`, `target`,
  `since` and `last_incident`; subsystem labels are now Database, Blob
  storage, Event bus, Task scheduler, Search index, Sign-in (with the enabled
  providers) and Upstream proxies.
- **System logs** (Operations → System logs, `system:cluster:admin`): each
  node keeps its newest `log.buffer_size` records (default 5000) in memory,
  scrubbed like stderr. Filter by node, level and text, follow with pause and
  refresh, open a line as JSON and download JSON lines.
  `GET /system/logs` supports cursors and long polling; other nodes answer
  over NATS. New setting `server.node_id`.
- **Log redaction** also covers headers and query values logged as maps
  (`Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, ...).

- **Settings → Authentication**: administrators configure OIDC and LDAP
  sign-in in the web UI or with `GET/PUT /identity-providers/oidc` and
  `/identity-providers/ldap`, without a restart: changes apply on every node
  (event bus plus a revision poll). Group → role mapping table with role
  pickers, the redirect URI to copy, and **Test** buttons (OIDC discovery,
  issuer, keys and a client-credentials check; LDAP connect, service bind
  and user lookup). Providers enabled in the configuration file win and are
  shown read-only. The login page follows the saved settings at once.
- **Named outbound proxies** (Settings → Proxies, `/proxies`): several
  proxies with credentials, no-proxy hosts, redirect target hosts and a CA
  bundle; one is the platform default. Proxy repositories pick *Platform
  default*, a named proxy (`config.proxy.http.proxy_name`) or *No proxy*.
  A proxy in use cannot be deleted (`409 in_use`). `http.proxy` from the
  configuration file is listed read-only as `config` and stays the default.
  **Test** fetches a URL through the proxy and reports status and latency.
- Secrets of these settings (OIDC client secret, LDAP bind password, proxy
  passwords) are sealed with `HANGAR_SECRET_KEY`, write-only in the API, and
  re-wrapped by `hangar admin rotate-secrets`. Changes are audited without
  secret values. Migration `00090` adds `settings`, `outbound_proxies` and
  `settings_revision`.
- **Overview traffic survives restarts and covers the cluster**: requests,
  downloads, response time, proxy cache hits and misses and proxy remote
  latency are added every 15 seconds by each node to per-minute rows in
  PostgreSQL (migration `00120`, kept 62 days) instead of living in process
  memory. The 1h / Today / 7 days / 30 days ranges read exactly those
  windows; `coverage_start` says when recording began.
- **Served from cache** counts every format and requests that reach a proxy
  through a group, once per client request; the card shows hits of total
  proxy requests and bytes saved, and says so when no proxy served content in
  the range. `cache.hits` and `cache.misses` are new in the overview API.
- **Upstream latency**: `GET /upstreams/health?range=` reports the median
  remote latency (`latency_ms`) and the number of remote requests
  (`requests`) recorded over the range; the Overview card follows the
  selected range. `upstream.status` events now name the repository, so the
  card updates the right row.
- **Group size**: groups report what they serve: `size_bytes` is their
  members' content with shared blobs counted once (nested groups resolved),
  `component_count` the distinct versions. The Repositories list shows
  "5.2 MB via 2 members" and the version count.
- "Published since yesterday" and the uploads in the traffic chart count
  hosted repositories only; proxy cache fills are no longer counted as
  publications.
- **UI shell**: menus and popovers sit on their own stacking layer (z-index
  scale in the design tokens), so page content such as the overview range
  switch no longer covers the account menu or notifications. The sidebar
  shows a static instance badge instead of an organisation switcher that did
  nothing; an unnamed instance reads "Hangar" and empty environment or region
  leave no stray separator.
- **Select fields**: one accessible listbox (keyboard, type-ahead, optional
  filter for long lists, form submission) replaces the native selects outside
  the package browser.
- **Connect a client**: per-format set-up commands that match the format
  guides (NuGet `index.json` with credentials, `.npmrc` with a token, pip,
  Maven `settings.xml`, Docker login, raw curl), publish commands for hosted
  repositories, the signed-in username filled in, and a link to create an
  access token. Everything is built from server-provided URLs.
- **Format marks**: Docker, npm, Maven, NuGet, PyPI, Helm and Go logos (Simple
  Icons, CC0; Maven under Apache 2.0, see `web/THIRD_PARTY_NOTICES.md`) in
  badges, tables, pickers and search facets, with a neutral icon for raw and
  any other format.
- **Repositories list**: a "Last activity" column (`last_activity_at`, from
  existing upload and download timestamps, no extra writes), type icons, and
  sorting by name, type, format or last activity (`GET /repositories?sort=`).
- **Repository browser**: the Packages page is a tree of repositories with
  each one's content laid out as clients see it (npm scopes and packages,
  Maven coordinates, NuGet ids and versions, PyPI projects, Docker images and
  tags, raw directories), loaded folder by folder, keyboard navigable, with a
  filter box and links that keep the selected node. Groups merge their
  readable members and name the member that holds each entry. The version
  picker is a versions table; no native selects remain.
- **Package page**: version card (newest, published or cached, downloads for
  the version and in total, size, vulnerability status, licences, home page,
  keywords), a "Set me up" panel with install commands per client, a Download
  button, and Readme, Versions, Files and Repositories tabs. READMEs are
  rendered as restricted Markdown (no raw HTML, http(s)/mailto links only,
  https images without a referrer).
- **File page**: path, repository (and the group it was reached through),
  package, content type, size, checksums, blob and file dates, downloads,
  the upstream of cached proxy files, a download link and **Delete file**.
- API: `GET /repositories/{name}/browse` (direct children of a folder,
  cursor-paged, one index seek per child), `GET /assets/{id}`,
  `DELETE /assets/{id}` (`repo:<name>:delete`, audited as `asset.delete`, the
  blob becomes eligible for compaction) and `GET /components/{id}/info`
  (README, description, licences, keywords and home page, read from the
  npm manifest or tarball, the `.nupkg`'s nuspec and README, PyPI core
  metadata or the Maven POM). The versions listing reports `size_bytes`.
- Migration `00110` adds `component_info`, the cache of extracted package
  facts (dropped with the version).

- **Vulnerability scanning** of npm, Maven, PyPI, NuGet and Go packages
  against the OSV advisory database (`scanning.osv.url`, default
  `https://api.osv.dev`, or a mirror) through the shared outbound HTTP client:
  batched `querybatch` requests with pagination, advisory details cached by
  modification time, rate limiting and retries with backoff. Severity from
  CVSS v4.0 / v3.x / v2 vectors or the database's own rating; *fixed in* from
  the affected ranges. Scans on arrival (hosted publish or proxy caching, in
  repositories with `scan_on_arrival`), on demand
  (`POST /components/{id}:scan`, `POST /repositories/{name}:scan`) and in the
  hourly built-in task `vulnerability-rescan`, which rechecks versions older
  than `scanning.rescan_interval` (default 24h). `scanning.enabled` turns it
  off. Docker images are labelled *image scanning not configured*.
- **Findings**: advisory id and aliases (CVE, GHSA), summary and text, CVSS
  score and vector, affected and fixed versions, references, first and last
  seen; findings no longer reported become *fixed* and re-open if they come
  back. Per-component scan status (never, ok, failed with the error, last
  scan time) in `ScanSummary` and new scan states `failed` and `unsupported`
  (also in search: `security:failed`; `cve:` now matches aliases).
- **Waivers** need a reason and an end date at most a year ahead
  (`system:security:write`), are audited, and re-open the finding when they
  expire. **Quarantine critical** (repository setting, off by default) blocks
  downloads of versions with an open critical finding and lifts the block
  when it is fixed, waived or the setting is turned off.
- New endpoints `GET /findings/{id}`, `GET /findings/trend`,
  `GET /scanning/status`; `GET /findings` gains `repository`, `q`,
  multi-value `severity`/`status`, `total`, `fixable`, `waived` and
  `components`; `GET /components/{id}/findings` reports the scan state.
- **Web: Vulnerabilities** replaces the placeholder: severity cards, 30-day
  trend, scanner coverage with *Scan repository now*, findings table with
  severity, status, fix and repository filters and search, and a finding
  drawer with references, waiver form, revoke and rescan. The Overview
  *Critical issues* card says when nothing has been scanned yet or scanning
  is off instead of showing *Clear*.
- Migration `00100` adds `component_scans` and the advisory columns of
  `findings`. Guide: [docs/guides/vulnerabilities.md](guides/vulnerabilities.md).

- **Single sign-on (OIDC)**: "Continue with SSO" through any OpenID Connect
  provider (`auth.oidc`): authorization code flow with PKCE (S256),
  `client_secret_basic` or `client_secret_post`, encrypted state cookie,
  ID token and nonce verification, userinfo fallback for missing claims,
  lazy discovery with retry. New endpoints `GET /auth/oidc/start` and
  `GET /auth/oidc/callback`; failures return to `/login?error=<code>`.
- **LDAP / Active Directory sign-in** (`auth.ldap`): the login form's
  directory realm and HTTP Basic for package clients, with service bind,
  escaped user filter, groups from `memberOf` or a group search, optional AD
  nested groups, LDAPS or StartTLS, and the local lockout and rate limits.
- **Group to role mapping**: `group_roles`, `default_roles` and
  `allowed_groups` per provider, plus role group mappings for providers
  `oidc` and `ldap`. Mapped roles are re-synced on every sign-in; roles bound
  by hand are kept. Keycloak full group paths (`/devops`) match. Unknown role
  names stop the server at start-up.
- **External users** are created at first sign-in (`auto_create`), linked by
  issuer and subject (OIDC), have no local password, and never take over an
  existing account with the same username (`account_conflict`).
- `GET /auth/providers` reports the OIDC button and LDAP toggle;
  `GET /identity-providers` lists both providers and their state.
- Migration `00080` adds `users.external_issuer` and
  `role_bindings.mapped_from`. Guide: [docs/guides/sso.md](guides/sso.md).

- **Releases**: version tags publish a multi-arch image
  (`ghcr.io/ismoilovdevml/hangar`, linux/amd64 and linux/arm64, smoke-tested
  against PostgreSQL), binaries for Linux and macOS with `SHA256SUMS`, and a
  GitHub Release carrying the compose stack and the installer.
- **Installer**: `deploy/install.sh` installs or upgrades the compose stack
  from a release image in one command (`--tls caddy` or behind an existing
  reverse proxy), generates secrets once without printing them, and supports
  `--uninstall [--purge]`. The registry host moved to
  `deploy/compose/sites/registry.caddy`.
- **Web: Packages** (`/browse`): repository tree with packages and their
  newest versions; package header with format, version selector, signed,
  quarantine and security state; install command per client (npm/pnpm/Yarn,
  pip/uv/Poetry, Maven/Gradle/pom, dotnet/PackageReference, Docker/Podman,
  curl/wget) with copy; Overview (digest, checksums, size, downloads, last
  download, licence, source), Versions (delete with confirmation, disabled
  without delete rights) and Files/Layers (checksums, downloads with the
  signed-in session). Dependents and Vulnerabilities tabs appear only when the
  server reports them.
- **Web: Search**: query box with qualifier and value completion, facets for
  format, security and repository that edit the query, highlighted results,
  sorting, paging, keyboard navigation and shareable URLs.
- **Web: Blob stores**: totals (used, dedup savings, growth and time to full,
  reclaimable), per-store usage against the soft quota and state; add
  filesystem or S3 stores (secret write-only); store page with the
  repositories using it, soft quota editing and deletion of empty stores.
- **Web: Repository settings** (`/repositories/<name>/settings`, opened from a
  repository row): the New repository form pre-filled, online/offline switch,
  delete with typed confirmation and copyable client configuration.

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
- **Container image**: the web UI is always embedded; a static
  `hangar-healthcheck` probe drives the `HEALTHCHECK` on the distroless,
  non-root image; builds are reproducible (build date from
  `SOURCE_DATE_EPOCH`, no VCS stamping, no build ids).
- **Production Docker Compose** (`deploy/compose`): Hangar, tuned
  PostgreSQL 16, MinIO with a bucket-only user, and Caddy with automatic TLS
  for the UI host and a separate registry host; secrets from `.env`
  (`HANGAR_SECRET_KEY` and `HANGAR_DOCKER_TOKEN_KEY` required), healthchecks,
  resource limits, read-only containers, `/metrics` kept off the public host,
  and an override for Docker port connectors.
- **Helm chart** (`deploy/helm/hangar`): stateless multi-replica Deployment
  with anti-affinity, PDB and optional HPA; migrations in a pre-install and
  pre-upgrade hook Job; optional bundled NATS JetStream cluster; Ingress
  presets for ingress-nginx and Traefik with a registry host, or Gateway API
  routes; `existingSecret` for every secret, with generated secret and token
  keys kept across upgrades; ServiceMonitor, NetworkPolicies, connector ports
  on the Service, `values.schema.json` and validation of unsupported
  combinations.
- **Operations**: backup and restore scripts (`deploy/scripts`: PostgreSQL
  dump plus an add-only blob copy for filesystem, S3 and the compose MinIO),
  a hardened systemd unit (`deploy/systemd/hangar.service`), deployment
  guides in `docs/deploy` (requirements and sizing, compose, Helm, binary,
  TLS and reverse proxies, corporate proxy, first administrator, backup,
  upgrades, high availability, troubleshooting), and a `hangar.example.yaml`
  that covers every configuration key. `make deploy-check` validates the
  chart, the compose stack and the scripts.

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
