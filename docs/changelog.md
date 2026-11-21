# Changelog

## Unreleased

## v0.2.0 (2026-11-21)

NATS is gone: events and jobs run on the PostgreSQL every node already
shares. Also new: webhooks, CI OIDC trusted publishing, `hangar apply`,
context-bound secrets, faster search, and signed releases with the Helm chart
on OCI. **Read the upgrade notes before upgrading**; full steps are in
[upgrade.md](deploy/upgrade.md#upgrading-to-020).

### Upgrade notes

- **Upgrade every node together.** v0.1 and v0.2 nodes share neither events
  nor jobs. With Helm, add `--set updateStrategy.type=Recreate --set
  updateStrategy.rollingUpdate=null` to this one upgrade to avoid a mixed
  window. Run the migrations (00310–00380).
- **NATS is no longer used.** Remove `nats:` and `HANGAR_NATS_*` (they are
  ignored, with one startup warning). Jobs still queued in JetStream are not
  carried over: drain the queue first (`hangar_job_queue_depth` = 0) to lose
  none; scheduled jobs run again on their next schedule. Afterwards the NATS
  server and its data can be removed:
  - Helm: the chart no longer deploys NATS; the upgrade deletes its
    StatefulSet, Services and Secret but keeps the PVCs (`kubectl delete pvc
    -l app.kubernetes.io/instance=<release>,app.kubernetes.io/component=nats`).
    Old `nats.*` values are accepted and ignored.
  - Compose: `install.sh` no longer creates or mounts `<data>/nats` and leaves
    an existing one in place for a rollback; remove it when you no longer need
    one.
- **PostgreSQL connections.** Each node holds one extra long-lived session for
  the event listener: budget `nodes × (database.max_conns + 17)`. PgBouncer
  must use session pooling, not transaction pooling.
- **System logs across nodes** are read over HTTP: each node's
  `server.node_url` (`HANGAR_SERVER_NODE_URL`) must be reachable from the
  others. The chart sets it to the pod IP and its NetworkPolicy allows
  pod-to-pod HTTP; elsewhere it defaults to the local address used to reach
  the database plus the port of `server.addr`.
- **Secrets are re-sealed as `hgs2`** (bound to their row and field) on the
  first start. **v0.1.x cannot read them afterwards**: to roll back, restore
  the pre-upgrade backup, or stop every v0.2 node and run `hangar admin
  rotate-secrets --downgrade` before starting v0.1.x (which also needs its
  NATS back).
- **Search** pages stop at 10,000 results. `pg_trgm` is created by the
  migration when the database owner may; otherwise Hangar warns at start and
  `CREATE EXTENSION pg_trgm; SELECT hangar_search_trgm();` adds the index
  later.
- **Retention.** New `tasks.run_retention` (default `2160h`, 90 days) prunes
  finished task runs; the first run after the upgrade deletes older ones (set
  `0s` to keep all). New `audit.retention` defaults to `0s`: audit history is
  kept unless you set a limit.
- **Dropped schema:** the unused `nodes`, `replication_targets` and
  `replication_state` tables and `task_runs.node_id`.
- **Metrics renamed:** `hangar_nats_connected` →
  `hangar_event_listener_connected`, `hangar_nats_reconnects_total` →
  `hangar_event_listener_reconnects_total`; new `hangar_job_queue_dead`.
- **Web UI addresses:** `/cluster`, `/settings/system`, `/settings` and
  `/access?tab=audit` redirect to the new System area, Audit log and settings
  pages.
- **APT and RPM groups with proxy members** need the remote's public keys in
  the proxy's `format.remote_keys`; without them the group answers 400.

### Events and jobs on PostgreSQL

- Cache invalidation and live events use LISTEN/NOTIFY on one dedicated
  connection per node; after a reconnect every cache resyncs and live
  streams restart.
- Jobs (task runs, scans, policy jobs) are claimed from a `jobs` table with
  `FOR UPDATE SKIP LOCKED` under a lease that long jobs renew; expired leases
  are reclaimed, failed jobs are retried with backoff and kept as dead for 7
  days. One job per type at a time per node, as in v0.1.9.
- APT and RPM reindexing and npm state cleanup are durable, debounced jobs run
  once per repository: deleting 1,000 packages triggers one reindex, not one
  per node per package, and nothing is lost when a node restarts.
- System logs of other nodes are fetched over signed HTTP.
- `/readyz` "eventbus" reports the listener; System status shows it with job
  counts.

### New features

- **Webhooks** (Settings › Webhooks, `/api/v1/webhooks`): component
  published, deleted, quarantined and released, repository created and
  deleted, and new scan findings, sent as generic JSON, Slack or Microsoft
  Teams messages. Deliveries are signed (`X-Hangar-Signature`, HMAC-SHA256),
  retried with backoff for about an hour from a PostgreSQL outbox, can be
  inspected and redelivered, and a webhook that keeps failing is disabled.
  URLs follow the outbound address policy of proxy remotes and are
  write-only; a URL on another host needs the secret again. New privilege
  `system:webhooks:read|admin`. See [webhooks](guides/webhooks.md).
- **CI OIDC trusted publishing** (Settings › CI trust,
  `POST /api/v1/auth/oidc/exchange`): GitHub Actions, GitLab CI and other OIDC
  issuers trade their ID token for a short-lived token scoped by a trust
  policy (issuer, audience, claim conditions, service account, scopes,
  lifetime up to 1 h). Tokens are single-use per ID token, never listed or
  rotated, and revoked when their policy changes. GitHub and GitLab policies
  must pin the owner. Managing policies needs `system:settings:admin` and
  every privilege the policy grants. See [CI OIDC](guides/ci-oidc.md).
- **Configuration as code**: `hangar apply -f FILE|DIR`, `hangar diff` and
  `hangar export` manage repositories (every format and type, with routing
  rules), blob stores, cleanup policies, roles and outbound proxies from YAML
  over the REST API. Plans are idempotent and ordered by dependency; secrets
  come from `valueFrom: {env|file}` and are never exported; `--prune` deletes
  only what the named apply set created or adopted (kept in the new
  `/resource-labels` API). JSON schema: `api/hangar-config.schema.json`. See
  [config as code](guides/config-as-code.md).
- **APT and RPM groups take proxy members.** A remote's packages are listed
  next to hosted ones under the group's signature after the remote's own
  signature (InRelease/Release.gpg, repomd.xml.asc) is verified with
  `format.remote_keys`; quarantine, routing rules and offline members are
  respected. The web UI has a "Remote signing keys" field.

### Security

- **Secrets bound to their place (`hgs2`)**: a sealed value copied to another
  row or field can no longer be opened there, closing the replay path that
  v0.1.9 blocked at the API. Key rotation and the startup sweep upgrade
  `hgs1` values; see [ADR 0010](adr/0010-context-bound-sealed-secrets.md).
- **Login and failed-authentication rate limits are cluster-wide** (new
  `rate_limit_buckets` table), not per node.
- Repository imports from APT, RPM, Cargo and PyPI sources bound
  decompression and memory; an oversized index fails the import instead of
  being read truncated.
- PyPI proxies hide quarantined cached files past the first 1,000 versions.
- Built with Go 1.27.4 (GO-2026-6859 in `net/http`); go-jose 4.1.6 (a denial
  of service when parsing tokens, reachable through the new CI OIDC
  exchange); the scanner image is based on Trivy 0.76.2 (a critical brotli
  fix in its base).
- Task run logs keep their order: a step could appear before "Started" when a
  heartbeat and a checkpoint wrote at the same time.

### Performance and reliability

- **Search** runs one statement over one match set with a trigram index for
  substrings: in a 100k-component test, an empty query went from 356 to
  127 ms and a query with no match from 17 to 0.6 ms.
- **Blob GC** lists candidates from two partial indexes (a batch went from
  23–33 ms to under 1 ms).
- **Audit partitions**: rows that landed in the default partition no longer
  block creating later months; months older than `audit.retention` are
  dropped when it is set.
- **S3**: small reads and writes take one request instead of HEAD plus
  request; publishing no longer calls storage inside its database
  transaction, and a blob GC run racing a publish no longer fails it.
- **Outbound HTTP** no longer takes a global lock per request; the client
  cache is bounded.
- System status shows OIDC and LDAP enabled from the web UI.

### Web UI

- **One System area** (`/system`: Status, Nodes, Logs, About) replaces
  Cluster, System status, System logs and Settings › System; users without
  cluster access land on About instead of an error. **One Audit log**
  (`/audit`); the Access page drops its Audit tab. The Settings hub is gone.
- **Dusk (dark) theme** is readable: every colour comes from tokens checked
  for WCAG AA in both themes.
- An empty server's Overview starts with **"Create your first repository"**.
- Admins of some repositories (e.g. `repo:npm-*:admin`) can create
  repositories on the default blob store.
- Vulnerabilities: lighter row actions; the quarantined count is no longer
  cut off. Search: one search box, examples from your own repositories. New
  task: errors at each field. Most downloaded keeps npm scopes and Maven
  groups. Large package trees no longer recompute on every scroll.
- One dialog, confirmation, filter field, error message and permission check
  everywhere; the permission check now matches the server exactly.
- Unhashed static files (login wallpaper, favicon) are revalidated with an
  ETag.
- The demo's numbers agree across pages.

### Releases, deployment and docs

- **Signed releases**: both images (every platform), the Helm chart and every
  release file are signed with cosign (keyless, bound to the release
  workflow); SPDX SBOMs and SLSA provenance are attached. See
  [verify a release](deploy/verify-release.md).
- **Helm chart on OCI**: `helm install hangar
  oci://ghcr.io/ismoilovdevml/charts/hangar --version 0.2.0`. Pod templates no
  longer repeat their selector labels (strict parsers such as kubeconform
  rejected them).
- The release gate also runs govulncheck, `npm audit`, Trivy on both images
  and kubeconform on the chart; Dependabot proposes weekly grouped updates.
- Every exported metric is documented (a test enforces it); the proxy
  upstream metrics and `hangar_routing_blocked_total` are on the dashboards.
- Docs: getting started lists all 11 formats and the real write policy
  default; README compares Hangar with Nexus, Artifactory and Harbor; a code
  of conduct, issue forms and a pull request template were added.

## v0.1.9 (2026-11-20)

A security and reliability release. Upgrading is recommended for every
installation; read **Upgrade notes** first.

### Upgrade notes

- **Repository-level proxies are checked like remote URLs.** A repository
  whose `config.proxy.http.proxy` points at a private, loopback or
  link-local address is refused on save and blocked when dialled. List the
  address in `http.allow_private_networks`, or define it as a named proxy
  (Settings › Proxies) and use `proxy_name`. Until then any other update of
  such a repository is refused too.
- **S3 blob stores managed through the API need static keys.** Creating,
  changing or testing an S3 store through the API without an access key is
  refused unless the new `storage.allow_ambient_credentials` is `true`
  (`HANGAR_STORAGE_ALLOW_AMBIENT_CREDENTIALS`). The default store from the
  configuration file and existing keyless stores keep working.
- **Sealed values are not accepted as input.** API, YAML and UI inputs
  refuse `hgs1.…` secrets (400); omit the field to keep the stored secret.
- **Moving a secret's target needs the secret again.** Changing an OIDC
  issuer origin, an LDAP URL, a proxy's scheme, host or port, or an S3
  endpoint (also in `:test`) requires `client_secret`, `bind_password`,
  `password` or `s3.secret_access_key` to be entered again.
- **Compose installs:** `secrets/` is mounted read-only and the one-time
  admin password moves to `<data>/bootstrap/initial-admin-password`.
  `install.sh` creates `bootstrap/` and moves an existing password; upgrade
  with `install.sh` rather than copying `compose.yaml` by hand.
- **Helm:** the chart is 0.1.9 and defaults to the 0.1.9 images (it pointed
  at 0.1.0, and `imageScanning.enabled` pulled a scanner tag that does not
  exist). `config.auth.oidc.client_secret` and
  `config.auth.ldap.bind_password` in values now fail the render; pass them
  with `extraEnv` from a Secret.
- **npm login tokens are scoped to the repository** (and a group's write
  member) and no longer reach other repositories or the REST API.
- **Unknown blob store names are an error**: a repository whose named blob
  store is missing no longer writes to the `default` store.
- The development compose project is now `hangar-dev`, so its old volumes
  (`hangar_postgres`, …) are no longer used.

### Security

- **SSRF through a repository proxy (critical)**: a repository's own
  `http.proxy` was added to the server-wide list of trusted proxy addresses,
  which skipped the outbound address guard for that address for every
  repository; a repository admin could reach cloud metadata or internal
  services. Only operator proxies are trusted now, and repository proxies
  pass the same policy as remote URLs.
- **Sealed secrets could be opened by replay (critical)**: a sealed `hgs1.`
  value copied from the database or a backup could be pasted into a
  repository or import task credential, and Hangar sent its plaintext to the
  configured host. Sealed input is refused everywhere a secret is accepted.
- **Stored secrets follow their endpoint**: OIDC, LDAP, proxy and blob store
  saves and connection tests no longer send a stored secret to a changed
  host.
- **Probe and S3 dials refuse cloud metadata addresses**; API-managed S3
  stores no longer sign requests with the host's ambient AWS credentials.
- **Image imports** send registry credentials only to the source's own
  origin or an https token service.
- **Sign-in providers cannot grant more than their editor holds**: saving an
  enabled OIDC or LDAP provider needs every privilege of its default and
  mapped roles (a settings admin could map new users to Admin).
- **Tokens minted through a token stay inside its limits**: a token created
  or rotated with an API or session token can't outlive it and inherits its
  allowed networks; rotating a token through itself no longer extends its
  life. Creating tokens in the web UI is unchanged.
- **Unlocking a user** needs the same coverage as resetting their password.
- **`GET /users/{self}/permissions`** with a scoped token shows only what
  the token can do.
- **npm login** counts against `auth.login_rate` (429 with `Retry-After`)
  and refuses users who can't read the repository.
- **Opening a Docker connector port** (`connector.http_port`) needs
  `system:repos:admin`.
- **Forwarded headers**: behind trusted proxies, `X-Forwarded-Proto` and
  `X-Forwarded-Host` come from the outermost trusted proxy's entry, not the
  client's.
- **IPv6 clients** are grouped by /64 for sign-in lockout and the login rate
  limit.
- **Event streams** (`/api/v1/events`) close within about 30 s of sign-out,
  session or token expiry or revocation, or the account being disabled; role
  changes apply to open streams.
- **NuGet proxy cache poisoning**: a proxied `.nupkg` whose manifest names a
  different id or version than the one requested is no longer cached under
  the manifest's identity (the client gets 502). Invalid ids in flat
  container and registration paths return 404.
- **Upstream URLs with dot segments** (`.`/`..`, also percent-encoded) taken
  from an index or a redirect are refused for every format, so they can't
  climb out of the remote's path with its credentials.
- **Archive bombs**: uploads are refused with 400 when the archive expands
  past a cap (Helm chart over 256 MiB, `.deb` control member over 256 MiB,
  `.crate` over 1 GiB); package info stops reading after 1 GiB.
- **RPM**: header lengths are no longer trusted for allocation (a 112-byte
  request could reserve 512 MiB); headers with more than 65,535 index
  entries are refused. Group metadata is kept only for roots that hold
  packages, so arbitrary paths no longer grow memory.
- **Quarantine fails closed**: if the quarantine lookup fails, downloads
  answer 500 instead of serving the file (Maven, PyPI, NuGet, Go, Cargo,
  Helm). Quarantined Maven non-unique SNAPSHOTs return 403. npm proxy and
  group packuments hide quarantined versions, and `latest` moves to the
  highest remaining one.
- **Offline write members**: group uploads to an offline write member return
  503 (PyPI, Go, Cargo, APT, RPM).
- **Delete-only users** can delete APT and RPM packages and unlist NuGet
  packages; the indexes are regenerated instead of failing with 403.
- **NuGet symbol packages** are no longer accepted (they were stored but
  never served): the service index has no `SymbolPackagePublish`, and a
  symbol push returns 400.

### Reliability

- **Stopping the server no longer hangs while a web UI tab is open**: event
  streams end when shutdown begins, instead of holding it for
  `server.shutdown_timeout`.
- **Docker connector ports** drain and stop together with the main port;
  stopping takes at most `drain_delay` + `shutdown_timeout`.
- **Deadlock fixed** (PostgreSQL 40P01) between download statistics and
  component deletes or publishes. While the database is unreachable,
  download counts for at most 10,000 assets are kept in memory; the rest are
  dropped with a warning.
- **Background jobs in HA**: each node takes one job of a type at a time, so
  idle nodes pick up the backlog. Image scans longer than 10 minutes finish
  instead of being retried and dropped (the per-job limit is
  `max(10m, 2×(scanner.timeout+1m))`, 32 minutes by default). On shutdown,
  running jobs are cancelled and returned to the queue.
- **Idle transactions**: database sessions idle inside a transaction for 5
  minutes are ended, unless PostgreSQL or the database URL
  (`idle_in_transaction_session_timeout`) sets otherwise.
- **Blob GC** deletes each blob in its own short transaction instead of
  holding a batch of row locks across storage calls;
  `storage.gc.batch_size` is now how many candidates are listed at a time.

### Web UI

- Dialogs keep their own size and padding; the new-token dialog scrolls with
  Create and Cancel always visible, and only the chosen expiry is
  highlighted.
- Search: Enter runs the search; a suggestion is inserted only when picked
  with the arrow keys or the mouse.
- ⌘K no longer opens behind an open dialog; the Cleanup notice's Dismiss
  button is reachable with a screen reader.
- Optional sections (token restrictions, upstream connection, sign-in setup)
  no longer close while you edit them.
- Fewer background requests: status and upstream health refresh once a
  minute and pause in hidden tabs; the sign-in page no longer re-renders
  every second.
- Package lists, search and findings refresh when packages are published,
  quarantined or deleted.
- The Overview shows the system status once, server messages show local
  times, counts have thousands separators ("76,992 versions"), and long
  package names wrap at dots and slashes.
- Placeholders for unbuilt features are gone (Dependents tab, Move
  repositories, Replication); the Cluster page is named Cluster.
- The demo shows the real release version and a clean sample README.

### Deployment, CI and observability

- **Releases run the full test suite by default** (set the repository
  variable `RELEASE_SKIP_TESTS=true` to skip), refuse a tag that doesn't
  match the chart's `appVersion`, and tag the scanner image only after its
  smoke test passes. Go tests under `deploy/` now run with the rest.
- **Helm**: NetworkPolicies for Trivy and the scan adapter; `helm test`
  works with `networkPolicy.ingressFrom`; the migration Job gets
  `extraEnv`, `extraEnvFrom`, `extraVolumes`, `extraVolumeMounts`,
  `podLabels`, `podAnnotations` and an existing ServiceAccount.
- **Installer**: a failed pre-upgrade backup is retried on the next run
  instead of being skipped; `GITHUB_TOKEN` is used for the version lookup,
  with a fallback when the API rate limit is hit.
- **Alerts and dashboards**: `HangarUpstreamCircuitOpen` resolves once a
  remote is idle; `HangarDown` matches any job containing `hangar`; "Remotes
  not healthy" leaves out remotes not used yet; the RSS and error panels are
  fixed. `make alerts-check` runs the alert rule tests when `promtool` is
  installed.

## v0.1.8 (2026-11-15)

- **Times are local, everywhere**: every timestamp in the web UI (Audit log
  rows and details, Access & audit, tasks, tokens, sessions, cluster, System
  status and logs, Settings) is shown in the browser's time zone with the
  zone named, e.g. "11 Nov 2026, 12:23 (UTC+5)", and the same instant in UTC
  on hover. The Audit log's custom range is entered in local time. Exports
  (CSV, NDJSON, log downloads) and cron schedules stay in UTC. Relative times
  carry the full time in their hover text.
- **Overview days start at the viewer's midnight**: `GET /metrics/overview`
  and `GET /upstreams/health` take `tz_offset` (minutes east of UTC), so
  "Today" no longer begins at 05:00 in UTC+5; the chart starts at the first
  recorded interval instead of a row of "not recorded" bars.
- **One byte unit**: sizes are shown in binary units (KiB, MiB, GiB), and
  durations in their short form (repository TTLs read "24h", not
  "24h0m0s").
- **Storage numbers that respect quotas**: the Overview's storage card and
  the quota reminder name the blob stores without a quota (and link to the
  one) instead of "no quota" while other stores have one. Blob store growth
  is averaged over the days the stores actually hold data
  (`growth_window_days`), and not extrapolated from less than a day.
  Deduplication savings under 1% read "<1%".
- **Error, empty and loading states**: an unknown address shows a 404 inside
  the app shell (with search and links); `/repositories/<name>` and
  `/packages` lead to the page meant. A page that fails to render shows
  "This page could not be shown" with Retry inside the shell, instead of the
  router's developer screen. Every "Could not load …" says why in plain
  words, with the server's answer under Details. A missing repository, blob
  store, task, package or version has one not-found page. Pickers say when
  their options failed to load (with Retry) instead of "none exist", tables
  show placeholder rows while loading, and live status pills say when the
  last update failed.
- **Errors in words, at the field**: a refused repository setting (such as
  a remote URL blocked by the outbound address policy) is shown under its
  field; a taken member or role name and an empty one say so under the
  name; a bad "keep tags matching" pattern is caught at the field;
  connection tests (proxies, LDAP, OIDC) give one sentence about the likely
  cause and the raw error once, under Details; search lists every problem in
  a query, warns about unknown qualifiers and formats, and no longer squeezes
  the error into the facet column.
- **API error messages**: validation messages no longer end in ": invalid
  argument", and duplicates read "… already exists" once. `POST
  /repositories` without `blob_store` uses the `default` store; messages name
  fields as the API spells them (`blob_store`, `http_port`).
- **Vulnerabilities**: a search that matches nothing no longer turns the
  banner into "No open vulnerabilities"; the findings pill reads
  "8 findings · 2 critical · CVSS 10.0" (breakdown on hover); the pull
  column and the in-use window are labelled apart; a rescan no longer moves
  the header or repeats its progress, and the scan line says why some
  packages are not counted ("60 of 65 scanned (5 unsupported)"). The package
  page keeps its Vulnerabilities tab and says why it is empty.
- **Locked accounts** show when they unlock ("unlocks at 12:12 (UTC+5), in
  14 min") instead of only the date.
- **Suggestion lists** (Audit log action, distribution release) use an
  app-styled list with keyboard support instead of the browser's own popup.
- **API error code for rate limits**: a remote's 429 surfacing through the REST
  API answers `rate_limited` (it was `upstream_unavailable`).
- **Docs**: registry logins stay valid until they expire after a token is
  revoked or a password changes; the failed-authentication limit also refuses
  valid credentials from the same address (CI behind NAT); the npm README's
  known limits match the cluster lock and tombstones.

- **Faster first load of the web UI**: the build writes brotli and gzip
  copies of its scripts, styles and page, and the server sends the one the
  browser accepts (`Vary: Accept-Encoding`; hashed assets stay cached for a
  year, the page is revalidated). Pages other than Overview load on first
  visit, so the first load fetches 178 KB instead of 1.09 MB. A boot screen
  shows while the scripts arrive; on a 500 kbit/s link with 400 ms latency
  it appears after about 0.8 s and the page after about 5 s, where the
  screen used to stay blank for 18.8 s. A tab opened before an upgrade
  reloads once when it asks for a script the new release no longer has (the
  server now answers 404 for it instead of the page). `openapi.json` is sent
  gzipped.
- **System logs keep the errors that matter**: the web UI's own successful
  reads (its files and a browser session's `GET` API calls, which every open
  tab repeats every few seconds) are logged at debug level, so they no
  longer fill the in-memory buffer. The newest warnings and errors (a fifth
  of `log.buffer_size`, at least 100) also outlive their place in it.
- **The node list no longer waits for slow checks**: `/system/nodes` and
  `/nodes` answer from the last check run and refresh it in the background,
  so a store probe waiting for its 3 s timeout does not hold up the System
  status and System logs screens. A client that cancels the request no
  longer logs "other nodes unavailable".
- **One source of truth for health**: `GET /system/status` is now the rollup
  of the System status checks (the same ones `/system/nodes` and
  `/system/health` report), so a broken blob store reads `down` in both, and
  its message names the failing check ("Blob storage down"). Signed-in
  callers get the failing checks in `problems`; their detail and the
  dependency errors need `system:cluster:read`. `/system/nodes` carries the
  same rollup in `summary`. The sidebar card, the Overview pill, the bell,
  System status, Settings › System and the Settings hub all show it, in grey
  while it loads or cannot be read; the bell lists the To do items.
- **Blob store state is real**: every store is probed each heartbeat (one
  hanging store no longer times the whole check out), and `/blob-stores`
  reports `ok`, `degraded` or `unavailable` with what the probe found
  (`state_detail`, `checked_at`) instead of a fixed `unknown`. A repository on
  an unavailable store is `offline` with `status_reason`
  `blob_store_unavailable`.
- **Proxy remotes are not "healthy" before they are used**: a remote this
  node has not contacted since it started is `unknown`; a request that fails
  (no answer or a 5xx) makes it `offline` at once with `last_error`, before
  the circuit breaker trips, and the next answer clears it. The repository
  list shows such a proxy as "Upstream unreachable" (`status_reason`
  `upstream_unreachable`, `status_detail`) instead of a green "Online".
  `hangar_upstream_state` gains `state="unknown"`.
- **Vulnerability scanner check**: System status lists the scanner (advisory
  scans that worked and failed, and whether the Trivy server answers).
- **Every sidebar entry is reachable on laptop screens**: sections fold from
  their heading and the browser remembers which (a section opens again on its
  own pages); when the list runs past the status card its ends fade and a
  button scrolls on; the current page's entry is scrolled into view.
- **Wide tables keep their key columns**: when a table does not fit its
  panel, secondary columns give way first (address, timestamps, descriptions)
  so the name, status and actions stay on screen; if it still has to scroll,
  a shadow marks the side with more columns. On phones each row becomes a
  card. Applies to repositories, scheduled tasks, the fix plan, members,
  tokens, roles, both audit logs, blob store, system and package tables.
- **Package detail gets the width**: the version and setup cards move under
  the package until it has room beside them, tabs wrap instead of clipping,
  advisory titles wrap, and the browse tree folds away with a button (also
  remembered).
- **Phone layouts**: the Overview header keeps the bell and account on the
  breadcrumb row; repository counts form one compact strip and the type tabs
  scroll on one row; search filters fold behind a "Filters" row; Packages and
  the API reference show their title before the tree or endpoint index;
  repository settings show Save under the settings and keep it in reach while
  there are unsaved changes; buttons and links on touch screens get 44 px
  targets.
- **Repository actions in the UI**: proxy settings offer "Invalidate cache"
  (with a confirmation and what was dropped), and repositories of a scanned
  format offer "Scan now". A package version can be scanned from its page.
  The Dependents tab is shown as planned; there is no SBOM download until the
  server serves SBOMs.
- **Scheduled tasks can be edited**: schedule and on/off on every task except
  cleanup policy tasks (which link to their policy), and tasks an
  administrator added can be deleted. New task adds blob compaction and Docker
  garbage collection, and an optional schedule for imports and pre-warms.
- **Blob store connection test**: `POST /blob-stores:test` checks an S3
  store's settings without saving them (bucket, write, read, delete; a failure
  names the S3 error code). Adding an S3 store tests it first; "Add anyway"
  saves a store that failed the test. S3 stores added in the UI get an
  "Edit connection" form (credentials, region, path style; the location only
  while the store is empty).
- **The default blob store shows the settings in effect**: its stored
  settings follow the server configuration and environment on every start
  (e.g. `HANGAR_STORAGE_S3_USE_PATH_STYLE`), and `from_config` marks it as
  defined by the configuration.
- **Blob stores page**: "Run compaction" on the reclaimable total, and
  "Move repositories" shown as planned.
- **Vulnerabilities summary**: severity cards (with how many have a fix), the
  quarantine policy of the repositories with the number of quarantined
  packages, and the open critical and high findings of the last 30 days.
- **Search finds pasted ids**: a bare advisory id (`CVE-2021-44228`,
  `GHSA-…`, `PYSEC-…`, `RUSTSEC-…`) searches as `cve:`, and a bare hex word of
  8 to 64 digits matches a digest prefix or the package name.
- **Command palette**: jumps to repositories, packages, advisories (by CVE or
  GHSA id) and users; a matching page ranks first and Enter opens it, the
  package search is the last option, and ArrowUp no longer wraps to
  "Sign out". "Create a token" is an action.
- **Routing suggestions** no longer say "already in place" for patterns that
  are in the form but not saved.
- **Development compose stack**: Hangar addresses the bundled MinIO
  path-style, so the default S3 store works.
- **"Connect a client" offers every format**: the dialog pages through
  `/me/client-endpoints` instead of showing the first three, and the
  endpoint per format prefers a group, then a proxy, then a hosted
  repository. Endpoints now carry `type`, `publishes` and `anonymous_read`.
- **Client snippets follow the repository type and work as copied**: Maven
  mirrors everything only to a proxy or group; a hosted repository is added
  by URL in an active profile, and every Maven snippet carries the URL; the
  token export is its own shell block, out of `settings.xml`. Cargo never
  names a registry `crates-io` (cargo's own source). Over plain http, NuGet
  sources get `--allow-insecure-connections`, pip trusts the host, Gradle
  sets `isAllowInsecureProtocol`, and Go says plainly that it sends
  credentials over HTTPS only (serve Hangar over HTTPS, or allow anonymous
  read) instead of a `.netrc` that is never sent. Install commands carry the
  signed-in user and, for pip and uv, the token. Checked with npm, pip,
  twine, uv, mvn, gradle, go, cargo, helm, dotnet, docker and curl.
- **Code is shown as typed**: no font ligatures in code (`<!--` stays
  `<!--`), and config blocks scroll sideways instead of wrapping mid-tag.
- **The package tree is repository, package, version**: folders lead down to
  packages, and a package opens to its versions (newest first, from every
  member of a group). Stored files moved to a repository's **Files** view
  (`/repositories/{name}/browse?view=packages` is the tree; `view=files`,
  the default, is unchanged). apt and rpm list their packages by name. Links
  that name a package (search results, shared URLs) open and select it in
  the tree. Long names are cut in the middle, and filtering the tree links
  to a search of every package.
- **Docker images are sized by their layers**: a version's size, its
  Versions row and the Layers tab come from the manifest (config plus
  layers; a multi-platform index by its linux/amd64 image, with every
  platform listed). The component detail has a new `image` field.
- **No single-file delete in Docker repositories**: layers and manifests are
  shared by tags; the UI no longer offers "Delete file" for them, and
  `DELETE /assets/{id}` answers 400 for docker files.
- **Delete a whole package**: the Versions tab deletes every version of a
  package after typing its name.
- **Raw files found by search open as files**, not as a package with a
  version and an empty README.
- **npm: the package state goes with the last version** deleted outside npm
  (the REST API, cleanup), so no package without versions is left behind
  unless it holds unpublish tombstones.
- **Overview numbers follow what the viewer may read**: without
  `system:cluster:read`, `GET /metrics/overview` counts only the
  repositories the caller may read (findings, storage, publications,
  rejected pushes) and leaves the instance-wide request, cache and capacity
  numbers null; the new `scope` field says which. The Overview labels the
  scoped numbers and says request statistics are for administrators.
- **Guessable passwords are refused**: new local passwords with fewer than 5
  different characters, a run such as `123456789012`, or a common password
  padded with digits and symbols (`Password2026!`) are rejected unless
  `auth.password.allow_weak` is true. The rule is listed live on the forced
  password change and on My account.
- **Sign-in says how many attempts are left**: a wrong password answers
  `details.attempts_left` and `details.lockout_seconds`; the sign-in page
  warns two attempts before the lock. A lockout only holds the username it
  was for, so another name can sign in at once; the page names the empty
  field, clears a wrong password, shows the system status and how to get a
  password reset.
- **A session that ends keeps the page**: on a 401 the app asks for the
  password in place instead of leaving the page, so unsaved edits survive;
  single sign-on runs in a new tab. A tab without a session goes to sign-in
  with "Your session ended".
- **The forced password change does not ask again** for the password typed
  at sign-in a moment ago (kept in memory only) and confirms the change.
- **Unsaved changes are guarded**: repository settings, the new repository
  form and cleanup policies ask before leaving with unsaved edits, and the
  browser asks before a reload.
- **Writes confirm themselves**: deleting a repository, version or file,
  waiving (with Undo), assigning, creating or saving roles and members, and
  saving a quota show a toast. Hidden Overview reminders can be undone and
  shown again.
- **Token reveal**: the one-time secret opens at the top with Copy focused,
  and closing before "I have stored the token" is ticked asks first.
- **Escape closes only the topmost dialog**, not the drawer under it.
- **Soft quota editor** offers GiB and TiB, shows a stored quota exactly
  (2 GiB stays 2147483648 bytes) and saves only what was changed.
- **Deleting a group** says that its members and their packages are not
  touched. The "is ready" note after creating a repository sits at the top
  of the summary instead of covering it.
- **Signed-in devices** are grouped by device and address, five at a time,
  with each session's expiry.
- **Demo build of the web UI**: `npm run build:demo` in `web/` builds the UI
  with its in-browser sample server into `web/dist-demo/`, served from
  `/hangar/demo/` on the project website. It starts signed in, every page and
  form works against sample data, and changes reset on reload. Links of the
  form `demo/#/repositories` open a page directly. The product build is
  unchanged and contains no mock code. `npm run site:screenshots -- <site-dir>`
  takes the website's UI screenshots from it.

- **Release checks are opt-in**: the test, lint and deploy-check jobs of the
  release workflow run only when the repository variable `RELEASE_TESTS` is
  `true`. When they run, images, binaries and the release wait for them and
  stop if one fails.
- **Overview success rate no longer counts authentication challenges as
  denied**: a 401 to a request that presented no credentials at all (NuGet
  and .NET clients, docker, npm and pip ask without credentials and retry
  with them) is counted in the new `hangar_http_auth_challenges_total` and
  stored apart (migration 00300); `denied` and the success rate leave it
  out, and the Overview note's tooltip says how many were not counted. A 401
  to wrong or expired credentials and every 403 still count as denied.
- **A client that hangs up is not a server error**: when the request is
  cancelled mid-way (dotnet probing workload manifests), every format answers
  499, logs at debug level instead of error, and the request is not counted
  as a 5xx. A remote or database timeout is still an error. Docker now answers
  500 instead of 401 when authentication fails for a reason other than the
  credentials.
- **NuGet `service_index` is treated as a secret**: some feeds carry their
  key in the path, so `config.format.service_index` is returned only to
  administrators of the repository (`***` to everyone else, the YAML export
  included); an update that sends `***` keeps the stored value, and error
  messages sent to clients no longer contain the path.
- **Proxy cache hit ratio**: a remote answering not found is no longer a
  cache miss. In a group whose first proxies' remotes do not have a package
  and whose last proxy serves it from its cache, the request is a hit; only
  content a remote supplied makes a miss. Index lookups that only locate a
  download do not count either.

## v0.1.7 (2026-11-09)

- **Releases are tested before they are published**: the release workflow
  runs `go test -race` against PostgreSQL, golangci-lint, the web lint,
  typecheck, unit tests and build, the installer tests and `helm lint` on
  the tagged commit, in parallel; images, binaries and the GitHub release
  wait for all of them.
- **CI workflow, ready to switch on**: the real-client e2e suite with every
  client in containers (`test/e2e/run-docker.sh`, `make e2e-docker`, image
  `test/e2e/runner/Dockerfile`) under `HANGAR_E2E_STRICT=1`, the S3 driver
  against MinIO, `make deploy-check`, Playwright against the mock build and a
  Playwright smoke test against a real server (`web/e2e/live`,
  `make web-e2e-live`), plus the image-scanning and LDAP e2e runners.
- **e2e suite**: works with Helm 4 (and still Helm 3) and on arm64 (the RPM
  proxy test picks the mirror for the machine's architecture);
  `HANGAR_E2E_STRICT=1` turns a missing client, credential or Docker daemon
  into a failure, `HANGAR_E2E_ALLOW_MISSING` names exceptions, and optional
  clients (buildx, crane, cosign, helm OCI) run as subtests that report a
  skip. Stronger checks: Maven artifacts are compared with what the client
  built and with Maven Central, a forced Helm overwrite on `allow_once` must
  answer 409 and leave the chart unchanged, and a raw proxy answering 502
  fails (the requirement on `http.allow_private_networks` is documented).
- **`make test` needs a test database**: without `HANGAR_TEST_DATABASE_URL`
  it stops instead of skipping a quarter of the Go tests;
  `HANGAR_TEST_SKIP_DB=1` runs anyway and reports the skip count.
- **API contract**: the six operations that answer 501 (`sbom`, `dependents`,
  `move-repositories`, replication targets) are marked `x-status: planned`
  with a documented 501 `not_implemented` answer; `docker_upload_cleanup`
  is in the `TaskType` enum. Tests check routes against `api/openapi.yaml`
  in both directions (including the sign-in, settings and signing-key
  routes) and that the web UI's mock server mocks only documented
  operations.
- **Test robustness**: queue, scheduler and task takeover tests assert on
  state instead of sleeping; web page tests allow for loaded CI runners.
- **Docs**: README lists the 11 formats; the roadmap, architecture and
  product spec say what is done, partial and planned; `CONTRIBUTING.md`
  explains the test suites; the changelog is split by release.
- **npm audit answers for real** (it always reported "0 vulnerabilities"):
  proxies pass the audit to their remote with only the names their routing
  rules allow, hosted repositories answer from Hangar's stored OSV findings,
  groups merge both. Bulk, quick and full audit endpoints, gzip bodies.
  When Hangar cannot vouch for a hosted version (not scanned yet, scanning
  off) or a remote fails, the audit fails (503/502) and npm says so instead
  of "found 0 vulnerabilities". `pnpm audit` works the same way.
- **npm search**: `/-/v1/search` on hosted (component search), proxy (remote
  search, routing rules applied) and group repositories.
- **npm logout** revokes the token `npm login` issued; personal access tokens
  are left alone. Tokens can be revoked through `core.TokenRevoker`.
- npm proxies serve tarballs whose upstream URL does not end in `.tgz`.
- PyPI: upload versions are compared by PEP 440 (`1.0` no longer matches a
  `10` file name); Simple API pages carry an `ETag` and answer `304`.
- Cargo: `foo-bar` and `foo_bar` are one crate, as on crates.io; publishing
  the other spelling answers `409`.
- Helm proxies leave malformed entries of a remote `index.yaml` out instead of
  failing the whole index; new metric `hangar_proxy_index_entries_skipped_total`.
- Docker: proxy tag lists follow the remote's `Link: rel="next"` paging; a
  push through a group no longer fails with `MANIFEST_BLOB_UNKNOWN` when a
  layer or platform manifest exists in another member: the content is linked
  into the write member.
- Proxy upstreams accept idempotent POST queries (`UpstreamRequest.Body`).
- The download-attribution wrapper keeps the component search, so NuGet,
  cargo and npm search use the full-text index in production.
- **Security fixes** (see `docs/security.md`, privilege escalation model and
  sign-in lockout):
  - Groups expose only what their author may read: adding a member needs
    `repo:<member>:read` on it and on everything a nested group member
    reaches, a write member needs `repo:<member>:write`, and opening a group
    to anonymous readers needs read on all of it. `system:repos:admin` alone
    no longer reaches content through a group. Refusals are audited.
  - User and role management cannot grant more than the caller holds:
    creating or changing roles, binding roles and creating members with roles
    need the caller to cover every privilege; setting a password, disabling,
    enabling, editing, deleting and signing out another account need covering
    everything it holds, so only holders of `system:*` manage administrators.
    `system:tokens:write` issues service-account tokens only for accounts the
    caller covers.
  - A password set by another member (reset or edit) must be replaced at the
    next sign-in.
  - `/v2/token` reloads the account before renewing a registry token: a
    disabled member, a revoked token or a removed role no longer renews it,
    and a renewal never outlives the first token of its chain.
  - Lockout is per username and client address (migration 00290), with a
    per-username slow-down (`auth.lockout.slowdown_after`, `max_delay`)
    instead of locking an account for everyone. Unknown usernames are counted
    and answered the same way, so `423` no longer reveals which accounts
    exist.
  - The server refuses to start with the public development keys of
    `deploy/docker-compose.yml` unless `server.dev_mode` is set (the
    development compose file sets it).
  - New installations protect `/metrics` with a generated bearer token
    (`secrets/metrics.bearer_token`, `metrics.auth: bearer`); a server with an
    open `/metrics` logs a warning on start.
  - **Upgrade notes**: groups created earlier by someone who could not read
    a member are not changed; review the members of your groups (and which
    are anonymous). Members whose password an administrator sets must now
    choose a new one at the next sign-in. Helpdesk-style roles with only
    `system:users:write` can no longer manage members who hold more than they
    do. Upgraded installations keep their `metrics.auth` (`none` unless you
    changed it). A server started with the development keys outside the
    development compose file must get its own keys, or `server.dev_mode`.
- **Fix: updating a proxy repository no longer drops its upstream
  credentials.** `PUT /repositories/{name}` without `config.proxy.auth`
  keeps the stored credentials; `"auth": null` removes them; an empty
  password or token with the same kind, username and header still keeps the
  secret. When `remote_url` moves to another scheme, host or port, stored
  secrets are not carried over: the request must resend them or set
  `"auth": null` (400 otherwise). The `repository.update` audit record notes
  `upstream_auth` (`added`, `removed`, `replaced`, `unchanged`), never the
  secret. The settings form sends `"auth": null` when authentication is set
  to none and asks for the secret again after a host change.
- **Upstream DNS cache** (`http.dns`, on by default): remote host names are
  resolved once per `ttl` (60s) with one shared query per name, a timeout or
  SERVFAIL is retried once, the last answer is served for up to `stale_ttl`
  (10m) while DNS fails, failures are remembered for `negative_ttl` (5s).
  Every address is still checked against the outbound address policy at
  check and dial time. Metrics `hangar_upstream_dns_cache_requests_total`,
  `hangar_upstream_dns_errors_total`, `hangar_proxy_upstream_dns_failures_total`.
- **Fix: DNS timeout bursts no longer open the circuit breaker.** Transient
  DNS failures count towards a remote's breaker at most once per second, so
  a CI burst failing on one lookup no longer turns a group into 502; a
  lasting DNS outage and NXDOMAIN still trip it as before.
- **Upstream 401/403 are named as credential problems.** Upstream health
  (`reason`), repositories (`status_reason`) and the System status upstream
  check tell `authentication failed (401)` and `access denied (403)` apart
  from an unreachable or policy-blocked remote, with a hint to check the
  proxy credentials; the Repositories list, repository pages and Overview
  show the same.
- **Web UI follows the user's roles**:
  - The sidebar, Settings and the command palette list only pages the user
    can open; empty sections disappear. Pages a role does not open (tasks,
    blob stores, cleanup, audit, access, cluster, system status and logs,
    new repository, new task, sign-in and proxy settings) say "You don't have
    access to … ask an administrator for `<privilege>`" without calling the
    API. A refusal the session did not predict shows the same note instead of
    "Could not load" and Retry.
  - Actions a role cannot take are hidden: New repository, Add blob store and
    blob store management, Take offline and Delete on repository settings.
    Repository creation counts `repo:<pattern>:admin` as well as
    `system:repos:admin`.
- **Web UI fixes**:
  - The token secret's copy button is readable (it was white on white).
  - Versions show one "v" (`v0.1.6`, not `vv0.1.6`) in the sidebar, sign-in
    page, Settings → System and the API reference.
  - Advisory text in the finding drawer is rendered as Markdown with the
    README renderer (no raw HTML, no images, restricted links), opens on its
    first section with **Show full advisory**, and code blocks scroll. Advisory
    titles render inline code and emphasis.
  - The Overview's status pill weighs the same signals as its cards
    (unreachable upstreams, server errors, node health); the sidebar card says
    "All nodes healthy".
  - The database backup reminder links the backup guide when the server has
    no backup task, and can be dismissed.
  - My account summarises repository rights (one row per distinct grant, with
    exceptions) behind **Show all N repositories**.
  - Repositories: cards on phones; the count cards stay totals under a name or
    format filter; creating a repository opens its page with a confirmation
    and **Connect a client**; the YAML preview shows `<format>` and `<type>`
    until they are chosen.
  - Cluster & replication shows the nodes from `GET /nodes`; replication is
    marked planned.
  - Most downloaded names the repository when a package repeats; avatars of
    `qa-bob` and `qa-help` read "QB" and "QH".
  - The UI no longer calls `GET /components/{id}/dependents` or
    `GET /system/config` (neither exists); Settings → System says where
    configuration lives.
  - The sign-in card no longer moves when an error appears; a 423 says "Too
    many failed sign-ins from this address" (or the server's message), not
    "This account is locked".
  - Accessibility: 24 px tap targets on phones, one `main` landmark on the API
    reference, unique table landmarks, heading order on Blob stores, and
    scrolling code blocks reachable by keyboard.

- **Move to Hangar: import and pre-warm tasks** (issue #74, guide
  `docs/guides/migrate.md`):
  - `repository_import` copies packages from any registry that speaks a
    format's client protocol into a hosted repository: npm, NuGet (V3, V2),
    Maven, PyPI, Go, Cargo, Helm, Docker/OCI (every platform), raw, APT and
    RPM. Name patterns, version ranges or globs, newest N, pre-releases.
    Identical versions are skipped, different ones reported as conflicts;
    publish dates, yanked, unlisted and dist-tag state are kept where the
    format has them. Rate-limited, concurrent, resumable from checkpoints.
  - `proxy_prewarm` fetches coordinates through a proxy or group so its cache
    holds them: pasted lists, lockfiles (`package-lock.json`,
    `pnpm-lock.yaml`, `yarn.lock`, `packages.lock.json`, `requirements.txt`,
    `uv.lock`, `poetry.lock`, `go.sum`, `Cargo.lock`, `gradle.lockfile`,
    `Chart.lock`) or a source registry's inventory. Reports cached, failed
    and skipped (routing-blocked names are skipped).
  - Source credentials are sealed with the secret key, never returned, kept
    on edits, swept by `rotate-secrets`, and guarded in the database
    (migration 00285). Creating, editing and running these tasks also needs
    write on the target repository.
  - API: task types and configs in `api/openapi.yaml`, `POST /lockfiles`
    reads a lockfile (gzip accepted). Imports keep the source's publish time
    through `core.WithPublishedAt`.
  - UI: **Scheduled tasks → New task** (import or pre-warm, lockfile upload),
    **Import** / **Pre-warm** on repository pages, run results with conflict
    and failure lists.
- **Blob storage integrity**:
  - A publish can no longer reference bytes that blob GC deleted. A publish
    that takes the first reference of a blob confirms, under the blob row
    lock and inside its transaction, that the bytes still exist; if GC
    removed them meanwhile, the upload fails with `404` (docker:
    `BLOB_UNKNOWN`) and can be retried, instead of leaving an image or
    package that never downloads again. Docker blob uploads and
    cross-repository mounts go through the same check.
  - Content that is stored but never published (a docker digest mismatch, an
    upload over a limit, a failed validation, a refused publish) is now
    reclaimed: every upload is recorded as an unreferenced blob, which a
    publish turns into a referenced one and blob GC otherwise deletes after
    `storage.gc.grace_period`. Uploading content again restarts the grace
    period of its unreferenced blob. Such uploads count as reclaimable bytes.
  - Failed and cancelled uploads leave nothing staged on either driver
    (filesystem temp files, S3 multipart uploads and temp objects), now
    covered by tests; crashed uploads are still swept by `blob.gc` after
    `storage.gc.temp_max_age`.
- **Proxy negative cache**: the per-remote cache of 404 answers is a bounded
  LRU (10,000 paths) with a per-entry expiry. A full cache evicts its least
  recently used path instead of forgetting every path at once, which sent a
  burst of requests to the remote. Evictions are counted in
  `hangar_proxy_negative_cache_evictions_total{repository}`.
- **Docs**: `docs/security.md` explains that other cluster nodes keep
  accepting an old password for up to `auth.basic_cache_ttl` after a change.
- **Write policy default, upgrade note**: a hosted repository created through
  the API without `write_policy` now gets `allow_once` (as the web UI already
  did), so a published release cannot be replaced by different bytes;
  `docker` keeps `allow`, because tags such as `latest` are meant to move.
  Existing repositories keep their stored policy, and an update that leaves
  `write_policy` out keeps it too. Scripts that relied on overwriting releases
  must now set `"write_policy": "allow"` explicitly. Re-publishing different
  bytes answers `409` in every format (identical bytes stay idempotent);
  Maven SNAPSHOTs remain redeployable.
- **NuGet**: a push of a version that already exists answers `409` under
  `allow_once` and `deny` (what `dotnet nuget push --skip-duplicate` skips),
  and a re-push under `allow` no longer relists an unlisted version. New
  `VulnerabilityInfo/6.7.0` resource, so NuGet Audit (`dotnet restore`)
  reports vulnerable packages: hosted repositories serve this server's open
  scan findings, proxies pass the remote's pages through (nuget.org publishes
  them), groups list their members' pages.
- **Quarantined versions are no longer advertised** in NuGet (flat container
  list, registration, search), Maven (`maven-metadata.xml` of hosted and
  proxy repositories; the build list of a quarantined SNAPSHOT answers `403`)
  and Go proxies (`@v/list`, `@latest` falls back to the newest other version,
  a query resolving to a quarantined version answers `403`). Clients now pick
  a version they can download instead of failing on its content.
- **Maven**: a non-unique SNAPSHOT file is replaced in one transaction (no
  window without the file, and no `delete` privilege needed). A proxy answers
  `.sha1`/`.md5` requests from the remote's side file instead of downloading
  the whole artifact first.
- **APT**: `Release` files declare `Acquire-By-Hash: yes` and every index is
  also served at `by-hash/SHA256/<digest>` and `by-hash/SHA512/<digest>`;
  superseded by-hash files stay for an hour, so `apt-get update` racing an
  upload no longer fails with "Hash Sum mismatch". A repository without
  packages (new, or after the last delete) serves a valid, signed, empty
  distribution instead of `404`.
- **RPM**: `repomd.xml.asc` always verifies the `repomd.xml` the same client
  was served, also when an upload replaces it between dnf's two requests.
  The same name-epoch-version-release.arch stored at two paths is listed once
  in `primary.xml` (the newest copy); under `allow_once` the second path is
  refused with `409`.
- **Raw**: concurrent uploads of `a` and `a/b` can no longer both succeed;
  the file/directory check and the write are serialized per top-level
  directory.
- **Routing rules against dependency confusion** (issue #73):
  - Proxy repositories take `config.routing.block` and `allow` name patterns:
    a blocked name is never requested from the remote nor served from the
    cache (also content cached before the rule). Groups take per-member
    `exclusive` (names served only by that member) and `block` patterns.
  - Enforced in every format (apt, cargo, docker, go, helm, maven, npm, nuget,
    pypi, raw, rpm): merged metadata, search and indexes leave out excluded
    versions, downloads answer 404 naming the rule, and the
    `X-Hangar-Routing` header explains exclusions. Remote requests of a proxy
    with rules must declare their package, so no format can leak an internal
    name upstream. APT and RPM proxies still pass the remote's signed indexes
    through; only downloads of blocked packages are refused.
  - Patterns match each format's identity: case-insensitive NuGet ids,
    normalized PyPI names, npm `@scope/*`, Maven `groupId[:artifactId]`, Go
    module path prefixes (GOPRIVATE syntax), Cargo crates, Docker image paths,
    Helm charts, APT/RPM package names, raw paths.
  - API: `GET/PUT/DELETE /repositories/{name}/routing`,
    `POST .../routing:test` (which member serves a name) and
    `GET .../routing:suggest` (patterns from what hosted repositories
    publish). Changes are audited as `repository.routing.update`; exclusions
    are counted in `hangar_routing_blocked_total` and audited as
    `routing.blocked` (at most once per name every 10 minutes).
  - Web UI: a **Routing rules** panel in proxy and group settings with a
    suggested default and a **Test a name** field.
  - Guide: `docs/guides/routing-rules.md`.
- **API and client feedback** (issues #67, #69, #70, #71, #75):
  - A request with a wrong, expired or revoked token or password answers
    `401` on every endpoint, public ones included: a NuGet service index, apt
    and rpm signing keys, cargo `config.json`, unknown paths and offline
    repositories of anonymous-read repositories, and `GET /system/status` or
    `GET /auth/providers` in the REST API. Each format answers with its usual
    challenge (`Basic realm="Hangar NuGet"`, `Cargo login_url=...`, ...); the
    REST API sends `WWW-Authenticate: Bearer realm="Hangar API"`. Requests
    without credentials are served as before, and the failures count towards
    `security.rate_limit.failed_auth`. The credentials are checked once per
    request. A NuGet push that sends a valid `X-NuGet-ApiKey` together with
    wrong Basic credentials now gets `401`.
  - Creating or updating a group with `blob_store` no longer fails: the field
    is ignored (logged at debug level) and left out of the answer.
  - The API guide and `/api/v1/automation-guide.md` explain the
    `X-CSRF-Token` header (the `hangar_csrf` cookie's value) for session
    writes and show the first sign-in from a script: `POST /auth/login`,
    `PUT /me/password`, then `POST /tokens`, plus `hangar admin token create`
    and `hangar admin reset-password`.
  - The OpenAPI document describes each format's `config.format` keys in its
    own schema (`NugetFormatConfig`, `GoFormatConfig`, ...), referenced from
    `RepositoryConfig.format` (`anyOf`, `x-format-schemas`); the wire format
    and generated client types stay compatible. A test keeps these schemas,
    and `RepositoryConfig`, in step with the Go structs.
  - npm scoped tarballs are advertised as `@scope/name/-/name-<version>.tgz`
    in every repository type, also for proxies of remotes that name them
    `@scope/name/-/@scope/name-<version>.tgz`; that form is still served, so
    existing lockfiles keep installing, and tarballs a proxy cached under it
    are reused.
- **Container image scanning with Trivy.** Docker and OCI images are scanned
  with a Trivy server in client/server mode when `scanner.trivy_url` is set;
  otherwise they still show *Image scanning not configured*.
  - A new scan adapter (`cmd/hangar-scanner`, image
    `ghcr.io/ismoilovdevml/hangar-scanner` built on the official Trivy image)
    runs the Trivy client for each image. It pulls the image from Hangar by
    digest with a registry token issued for that one repository: pull only,
    valid for 15 minutes, not renewable, able to read quarantined content,
    not counted as a download. The Hangar process runs no command.
  - Every platform manifest of a multi-platform image is scanned
    (`scanner.platforms: default` scans only `linux/amd64`); findings are
    merged per advisory, package and file with their platforms. Helm charts
    and other OCI artifacts are reported as *Not a container image*.
  - OS packages and application libraries land in the same findings tables
    (source `trivy`) with severity, CVSS, *fixed in*, the layer, the file of a
    library and the distribution's verdict. Arrival scans, the hourly rescan,
    `:scan`, waivers, assignment, quarantine, search and the Overview card
    work for images.
  - The fix plan has one row per image with its tags and **Rebuild · N
    packages**; **Copy upgrade** copies a Dockerfile line for the image's
    distribution (`RUN apk upgrade --no-cache ...`). The package page and the
    drawer show where a package sits in the image.
  - `GET /api/v1/scanning/status` reports the Trivy server version and its
    database date (`images`).
  - Settings: `scanner.trivy_url`, `trivy_token`, `adapter_url`,
    `adapter_token`, `registry_url`, `platforms`, `timeout`, `workers`.
    Migration `00250` adds `findings.location` and `findings.image`, widens
    the finding key to the location and allows `unsupported` scan rows.
  - `install.sh --with-trivy` (asked on a first interactive install) adds the
    compose profile `trivy` with the database in `<data>/trivy/`, generates
    `secrets/scanner.adapter_token` and wires `scanner.*`; `--trivy-proxy`
    sends the database downloads through a corporate proxy and
    `--without-trivy` turns it off. The Helm chart gets `imageScanning`.
    Guide: `docs/guides/vulnerabilities.md#container-images`.
- **Faster repository list** (issue #68): `GET /api/v1/repositories` and
  `GET /api/v1/repositories/{name}` no longer aggregate every asset on each
  request. Database triggers log each asset and component change in the
  writer's transaction, and the totals are folded into stored rows: by a
  background loop every 5 s, and before each read, so a client sees its own
  uploads at once. Groups keep reference counts of the blobs and versions
  their members serve, so a shared blob or version is still counted once.
  Every hour the totals are recounted from scratch; any difference is
  repaired and logged as a warning. With 50 repositories and 100k assets the
  list drops from about 440 ms to 2 ms (median). Migration 00275 adds the
  tables, the triggers and an index for last activity; the first fold after
  the upgrade counts the existing content (about 1.5 s per 100k assets).
- **Client cancellation is not an error**: an API request whose client goes
  away (for example by leaving a page) is logged at debug level and answered
  with status 499. It is no longer logged as `ERROR ... context canceled`
  or counted as a server error. The access log also logs a 5xx caused by a
  departed client at debug level.
- **Search**: the total and the facets of dimensions the query does not
  filter on come from one statement instead of four. `GET /api/v1/search`
  takes about 35% less time with 33k components.
- **Cluster operations** (issue #34):
  - `nats.replicas` (1 to 5, default 1) sets the replica count of the
    JetStream job stream. Every node creates or scales the stream on start,
    so changing it updates an existing stream in place; embedded NATS accepts
    only 1. Helm: `nats.streamReplicas`, which defaults to
    `nats.bundled.replicas`, so **upgrading a release with the bundled NATS
    cluster scales the job stream from 1 to 3 replicas**.
  - Migrations hold a PostgreSQL advisory lock (per schema). Nodes that start
    together with `migrate_on_start`, or `hangar migrate up` next to starting
    nodes, no longer fail with `duplicate key value violates unique constraint`:
    one process migrates and the others wait, then find nothing pending.
    Nodes without `migrate_on_start` wait for a running migration instead of
    refusing to start. The Helm chart now allows `migrations.mode: onStart`
    with several replicas. The lock needs a session-level connection (not
    PgBouncer in transaction pooling mode), and `database.max_conns` must be
    at least 2.
  - `/metrics` authentication: `metrics.auth` `none` (default), `bearer`
    (`metrics.bearer_token` or `metrics.bearer_token_file`) or `basic`
    (`metrics.username`, `metrics.password` or `metrics.password_file`), plus
    an optional `metrics.allowed_cidrs` allowlist that uses the trusted-proxy
    aware client address. Credentials are compared in constant time and count
    towards the failed authentication limit. Helm: `metrics.auth.mode` and
    `metrics.auth.existingSecret`, also used by the ServiceMonitor. Scrape
    configurations: `docs/operations/monitoring.md#protecting-metrics`.
  - `server.drain_delay` (0s to 5m, default 0s): after SIGTERM a node keeps
    serving with `/readyz` failing (and `Connection: close`) before the
    listener closes, for load balancers that poll `/readyz`.
    `server.shutdown_timeout` is now validated (1s to 24h). Kubernetes
    guidance: `docs/deploy/high-availability.md#draining-nodes`.
- **Vulnerability scanning of APT and RPM packages by distribution release.**
  A repository's `config.scanning.distro` (an OSV ecosystem such as
  `Debian:12`, `Ubuntu:22.04:LTS`, `Rocky Linux:9`, `AlmaLinux:9`,
  `Red Hat:enterprise_linux:9::baseos`; **Distribution release** in the
  repository settings, a picker with the common releases plus free text)
  names the release whose advisories apply. Proxies of Debian and Ubuntu
  archives detect it from the suites clients fetch, Rocky Linux, AlmaLinux and
  Red Hat proxies from the remote URL; the setting wins. Packages are checked
  by source package (Debian `Source:`, with the source version of binNMUs) or
  source RPM, versions are ordered like dpkg and rpm do, and the fix plan
  suggests `apt-get install --only-upgrade` / `dnf upgrade` for the installed
  binaries. A repository whose release is set or changes is rescanned.
  Migration `00265` adds `repository_distros`.
- **OIDC sign-out ends the provider session too** (RP-Initiated Logout).
  `auth.oidc.logout: provider` (default; *Sign out* in Settings →
  Authentication) sends the browser to the provider's `end_session_endpoint`
  with `id_token_hint`, `client_id` and `post_logout_redirect_uri` =
  `<base_url>/login`; `local` ends the Hangar session only, as does a provider
  without the endpoint. The ID token is stored with the session, sealed with a
  key derived from the session cookie (migration `00266`). `POST
  /api/v1/auth/logout` answers `200 {"logout_url"}` for a provider sign-out,
  `204` otherwise. Register `<base_url>/login` as a post-logout redirect URI
  (Keycloak: *Valid post logout redirect URIs*). Back-channel logout is not
  supported.
- **LDAP / Active Directory acceptance test.** `test/e2e/ldap/run.sh` runs
  `TestLDAP` (build tag `e2e`) against OpenLDAP and a Samba AD domain
  controller in Docker: sign-in, group mapping with AD nested groups,
  StartTLS and ldaps with certificate checks, wrong passwords, lockout, Basic
  auth from a package client and the settings *Test* endpoint.

## v0.1.6 (2026-11-09)

- **Web UI: format settings and signing keys.** The repository form edits
  APT `architectures`, RPM `repodata_depth`, the APT/RPM `signing_key`, Go
  proxy `sumdbs` and `sumdb_via`, Cargo proxy `download_url` and Helm proxy
  `related_hosts`, each only for the format and type it applies to and checked
  like the server does. APT and RPM settings pages get a **Signing key** panel:
  fingerprint, algorithm, own or shared key, `public.gpg` / `public.key`
  links, rotation with a confirmation, and a write-only key import. Saving a
  proxy no longer drops its `related_hosts`, `offline` and
  `max_concurrent_requests`.
- **APT and RPM repositories** (`apt`, `rpm`): hosted, proxy and group repositories
  for Debian/Ubuntu and RHEL/Rocky/AlmaLinux/Fedora.
  - APT: `.deb` uploads (raw `PUT` or multipart), Packages/.gz/.xz per
    distribution, component and architecture, and a Release with every hash.
    InRelease and Release.gpg are signed with the repository key.
  - RPM: `.rpm` uploads with the header parsed in Go, regenerated createrepo
    repodata (sha256, `rpm:header-range`), `repomd.xml.asc` for
    `repo_gpgcheck=1`, and an optional per-directory layout (`repodata_depth`).
  - Proxies pass the remote's signed metadata through unchanged. Index files
    are checked against the cached Release or repomd.xml; by-hash files,
    repodata files and packages are cached forever.
  - Groups combine hosted members only and are signed with the group's key.
  - Signing keys are OpenPGP (RSA 4096 by default, or Ed25519), one per
    repository or one shared key, sealed with `security.secret_key`.
    `GET`/`PUT /api/v1/repositories/{name}/signing-key` and
    `POST .../signing-key:rotate` show, import and rotate them; rotations and
    imports are audited. `hangar admin rotate-secrets` re-wraps them.
  - Guides: `docs/guides/apt.md`, `docs/guides/rpm.md`.
- **Helm chart repositories** (`helm`): hosted, proxy and group. `helm repo add`,
  `helm install`, `helm pull --verify` with provenance files, `helm cm-push` and
  `curl --upload-file` uploads, a generated and cached `index.yaml`, and the
  ChartMuseum read API. Guide: `docs/guides/helm.md`.
- **Go modules** (`go` format): the GOPROXY protocol (`@v/list`, `.info`,
  `.mod`, `.zip`, `@latest`, case-encoded paths) for hosted, proxy and group
  repositories. Proxies of `proxy.golang.org` or any GOPROXY cache versions
  forever and lists for `metadata_ttl`; 404/410 keep `GOPROXY=a,b` fallback
  working. Hosted modules are published with `PUT .../@v/<version>.zip`
  (validated against the module zip rules; optional `.mod` and `.info`),
  write-once under `allow_once`, deleted with `DELETE .../@v/<version>`.
  Proxies pass the `sum.golang.org` checksum database through. Browse tree,
  README and licence from the module zip, OSV scanning. Guide:
  `docs/guides/go.md`.
- **Cargo format**: Rust crate registries over the sparse index protocol.
  Hosted repositories take `cargo publish` (crate name and SemVer rules,
  tarball checks, versions immutable), `cargo yank` / `--undo`,
  `cargo search` and `cargo owner --list`; index files carry ETag and
  Last-Modified. Proxies cache crates.io index files for `metadata_ttl` and
  `.crate` files forever (verified against the index checksum); groups merge
  members per version and send publishes to the write member. Private
  repositories announce `auth-required`, cargo's raw token and `Bearer` are
  accepted. README, licence and repository come from the `.crate`; crates are
  scanned against OSV `crates.io`. Guide: `docs/guides/cargo.md`.

## v0.1.5 (2026-11-05)

- **Automation-friendly API**: the OpenAPI document is public at
  `/api/v1/openapi.yaml` and `/api/v1/openapi.json`; `/api/v1/automation-guide.md`
  and `/llms.txt` give scripts and automated clients a short guide with the
  server's real URLs (authentication, error body, paging, rate limits, common
  calls). `GET /me` (same body as `GET /auth/session`) now includes a `token`
  section with the token's scopes, restrictions and expiry. Requests to paths
  the API does not have answer `404 not_found` instead of `501`.
- **API reference page** at `/api-docs` (login footer, avatar menu,
  Settings, ⌘K): every endpoint grouped by area, parameters, bodies,
  responses and schemas, search, copyable curl commands for this server, and
  "Try it" with the current session or a pasted token. Public; built in, no
  third-party renderer.
- **Token creation, redone**: permission presets (read only, publish to
  selected repositories, repository admin, security reviewer, full admin,
  custom), a repository picker with glob patterns, a custom editor with a
  live plain-language summary, and greyed-out choices beyond the owner's own
  rights. After creation the secret comes with paste-ready snippets: curl,
  `HANGAR_URL` / `HANGAR_TOKEN`, npm, NuGet, pip, Maven and docker
  configuration, and a block for automation.
- **Token restrictions**: optional `allowed_cidrs` (client networks) and
  `read_only` (scopes narrowed to read; the API refuses writes with
  `403 read_only_token`); registry logins inherit them. Migration `00210`.
- **Token rotation**: `POST /tokens/{id}/rotate` and a Rotate button create a
  replacement with the same scopes, restrictions and lifetime; the old token
  is revoked now, after a grace period, or kept. Audited as `token.rotate`.
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
- **First administrator without configuration**: on the first start of an
  empty installation without `bootstrap.admin_user`, Hangar creates the local
  user `admin` with a generated one-time password, printed once to standard
  error (never into the System logs buffer) and written to
  `bootstrap.initial_password_file` (mode 0600) when set. A password already
  in that file is used instead. The file is deleted after the first password
  change. Only one node creates the user. See
  [first administrator](deploy/first-admin.md).
- **Required password change**: the generated administrator, administrators
  from `bootstrap.admin_user` / `hangar admin bootstrap` (unless
  `bootstrap.require_password_change: false`), users given a one-time
  password by the new `hangar admin reset-password --user NAME`, and members
  an administrator flags with **Require password change**
  (`POST /users/{id}/require-password-change`) must set a new password before
  anything else. Until then their password sessions reach only
  `GET /auth/session` (`password_change_required: true`), `PUT /me/password`
  and logout; everything else answers 403 `password_change_required`, package
  clients and token creation are refused, and the UI shows only a
  **Set a new password** page with live policy checks. The change re-issues
  the session and is audited as `user.password.changed` (`reason:
  first_login`). Members show a **Must change password** badge
  (`must_change_password`); the session reports `password_policy`. Migration
  `00200`. Changing your own password now always re-issues the session
  cookie.
- **Behaviour change**: administrators created from
  `HANGAR_BOOTSTRAP_ADMIN_USER` / `HANGAR_BOOTSTRAP_ADMIN_PASSWORD` or
  `hangar admin bootstrap` must now change their password at the first
  sign-in. Set `HANGAR_BOOTSTRAP_REQUIRE_PASSWORD_CHANGE=false` for automation
  that keeps using the configured password.
- **Two-question install**: `curl -fsSL
  https://github.com/ismoilovdevml/hangar/releases/latest/download/install.sh
  | sudo bash` asks for a data directory (default `/srv/hangar`) and an
  optional domain (Caddy with automatic TLS, or behind your reverse proxy),
  generates every secret, starts Hangar and PostgreSQL and prints the URL,
  the user `admin` and the one-time password the server generated
  (`bootstrap.initial_password_file`, `<data>/secrets/initial-admin-password`). Every question has a flag
  (`--data-dir`, `--domain`, `--tls caddy|proxy`, `--port`, `--yes`).
  Re-running it upgrades in place after dumping the database to
  `<data>/backups/`; `--uninstall` keeps the data, `--purge` deletes it after
  a typed confirmation; `--files-from` and `--mirror` serve offline hosts.
- **One data directory, no named volumes**: the compose stack bind-mounts
  `config/`, `secrets/`, `postgres/`, `blobs/`, `nats/` and `caddy/` of the
  data directory with SELinux labels and the container users' ownership.
  The installer-managed `.env` holds no secrets. MinIO is no longer part of
  the compose stack, and the release archive `hangar-compose.tar.gz` no
  longer contains `env.example`.
- **Secrets from files**: new setting `secrets_dir` (`HANGAR_SECRETS_DIR`):
  a file named after a setting's path (`security.secret_key`,
  `database.url`, `auth.oidc.client_secret`) holds its value; the
  environment still wins. Works with Kubernetes secret volumes.
- **Moving `/opt/hangar` installations**: `install.sh --migrate-from
  /opt/hangar --data-dir /srv/hangar` (offered automatically when found)
  converts the `.env` secrets into files, keeps `hangar.yaml`, the URL, port,
  bind address and trusted proxies, copies the `hangar_postgres-data` and
  `hangar_hangar-data` volumes, starts the new layout, checks `/readyz` and
  prints rollback steps; the old directory and volumes are left untouched.
  The installer flags `--dir`, `--storage`, `--s3-endpoint` and
  `--admin-user` are gone.

## v0.1.4 (2026-11-05)

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

## v0.1.3 (2026-11-04)

- **System status** (Operations → System status, `system:cluster:read`): every
  node with its URL, version and commit, uptime, last heartbeat and scheduler
  role, and the health of PostgreSQL, blob stores, NATS / JetStream, the task
  scheduler, the search index, external sign-in and upstream proxies, each
  healthy, degraded or down with details. Nodes publish their checks every
  15 s into `system_nodes` (migration `00140`); a silent node shows as not
  responding. New endpoints `GET /system/nodes` and `GET /system/health`;
  `GET /nodes` is implemented from the same heartbeats and
  `/system/status` reports real `nodes_up` / `nodes_total`.
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

## v0.1.2 (2026-11-04)

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

## v0.1.1 (2026-11-04)

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

## v0.1.0 (2026-11-04)

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
