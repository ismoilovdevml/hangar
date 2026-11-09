# Vulnerability scanning

Hangar checks every npm, Maven, PyPI, NuGet, Go and Cargo package version it
stores, and the APT and RPM packages of repositories whose distribution
release is known, against the public [OSV](https://osv.dev) advisory
database (or a mirror of it) and lists what it finds on the **Vulnerabilities** screen as a fix plan
(what to upgrade, and whether builds still pull the vulnerable versions), on
the package page and in search (`security:findings`, `cve:CVE-2024-21907`).

Docker and OCI images are scanned with [Trivy](https://trivy.dev) when a
Trivy server is configured ([Container images](#container-images)); without
one they show *Image scanning not configured*, never *Clean*. Raw files have
no advisory source and show *No advisory source*, and so do APT and RPM
packages until their repository names a
[distribution release](#apt-and-rpm-distribution-releases).

## How it works

| When | What is scanned |
|---|---|
| A package arrives (hosted publish or a proxy caching it) | That version, in repositories with **Scan on arrival** (`scan_on_arrival: true`) |
| Every hour, task `vulnerability-rescan` | Versions never scanned, versions whose last scan failed (after 30 minutes), and versions whose last scan is older than `scanning.rescan_interval` (default 24 h), in every repository, so new advisories are picked up |
| On demand | Everything: **Rescan** on the Vulnerabilities screen or `POST /api/v1/vulnerabilities/rescan`. One repository: `POST /api/v1/repositories/{name}:scan`. One version: `POST /api/v1/components/{id}:scan` |

For each version Hangar asks OSV `POST /v1/querybatch` (up to 1000 versions
per request, result pages followed) and then `GET /v1/vulns/{id}` for each
advisory it has not seen at that modification time. Requests are rate
limited (10 per second) and retried with exponential backoff on network
errors, `429` (honouring `Retry-After`) and `5xx`.

A scan that fails keeps the findings of the previous scan and records the
error; the package shows *Scan failed* and the hourly task retries it.

Package names are sent the way advisories name them: npm `@scope/name`, Maven
`groupId:artifactId`, NuGet the original-case package id, PyPI the project
name, Go the module path, APT the source package and RPM the source RPM (see
below).

### APT and RPM: distribution releases

Distribution advisories apply to one release: OSV keeps `Debian:12` and
`Debian:13`, `Ubuntu:22.04:LTS` and `Ubuntu:24.04:LTS` apart, and the same
`openssl` build is fixed in one and not the other. An APT or RPM repository
is therefore scanned only once its release is known:

- **Setting**: *Distribution release* in the repository settings
  (`config.scanning.distro`), an OSV ecosystem. The picker offers the common
  ones; any other OSV ecosystem of the format is accepted.

  | Format | Examples |
  |---|---|
  | APT | `Debian:11`, `Debian:12`, `Debian:13`, `Ubuntu:20.04:LTS`, `Ubuntu:22.04:LTS`, `Ubuntu:24.04:LTS`, `Ubuntu:24.10` |
  | RPM | `Rocky Linux:8`, `Rocky Linux:9`, `AlmaLinux:9`, `Red Hat:enterprise_linux:9::baseos`, `Red Hat:enterprise_linux:9::appstream`, `openSUSE:Leap 15.6` |

  Red Hat keeps BaseOS and AppStream apart: use one repository per stream.
- **Detected** for proxies when the setting is empty: an APT proxy of a
  Debian or Ubuntu archive (`deb.debian.org`, `security.debian.org`,
  `archive.ubuntu.com`, `ports.ubuntu.com`, or a mirror whose path ends in
  `/debian` or `/ubuntu`) takes the release of the suites its clients have
  fetched (`bookworm` and `bookworm-updates` are `Debian:12`, `jammy` is
  `Ubuntu:22.04:LTS`); a proxy that serves suites of two releases is left
  alone. An RPM proxy of a Rocky Linux or AlmaLinux mirror takes the major
  version in its URL (`https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os`
  is `Rocky Linux:9`), a Red Hat CDN URL its release and stream. Detection
  runs every ten minutes and when the repository changes; the setting always
  wins.

When the release of a repository is set or changes, the repository is
rescanned. What is asked:

| | Package name | Version |
|---|---|---|
| APT | the source package (`Source:` of the control file, else the package): `libssl3` is checked as `openssl` | the source version when `Source:` names one (binNMUs), else the package version, with its epoch |
| RPM | the source RPM name (`openssl-libs-3.0.7-27.el9` comes from `openssl-3.0.7-27.el9.src.rpm`) | `[epoch:]version-release` of the package |

Versions are compared the way the package manager does (`dpkg
--compare-versions`, rpm's `rpmvercmp`) for *Fixed in* and the fix plan, so
`2.36-9+deb12u4` fixes `2.36-9+deb12u3` and an epoch outranks everything.
The fix plan's command upgrades the installed binary packages:
`apt-get install --only-upgrade libssl3 openssl`, `dnf upgrade openssl-libs`.

Packages you build yourself and upload to a hosted repository are usually
not in the distribution's advisories (unless they reuse a distribution
package name); leave the setting empty for such repositories.

### Severity

The severity is computed from the advisory's CVSS vector: CVSS v4.0 first,
then v3.1 / v3.0, then v2 (critical ≥ 9.0, high ≥ 7.0, medium ≥ 4.0, low
below). A severity given for the specific package wins over the advisory's.
Advisories without a vector use the database's own rating
(`database_specific.severity`, e.g. GitHub's `MODERATE`); with neither the
severity is *unknown*.

### Fixed in

*Fixed in* is the lowest version that fixes the version you have, taken from
the advisory's affected ranges (the range that contains your version; an
advisory that lists affected versions explicitly is matched exactly). *No fix
yet* means no fixing release is known.

## The fix plan

The Vulnerabilities screen answers "what do we upgrade first?". Its header
counts the open findings by severity and shows how many packages are scanned
and when OSV was last asked; **Rescan** (needs `system:security:write`)
queues a scan of every repository and shows the progress until each package
that existed when it started has been checked (a run that has not finished
after 3 hours is reported as stopped).

The banner sums up the impact: *Upgrading 6 packages fixes 22 of 33 findings
that builds actually use*. A package version is **in use** when it was
downloaded within `scanning.in_use_window` (30 days by default); the bar
shows the findings in used versus unused packages.

The table has **one row per package** (format and package name, across
repositories):

| Column | Meaning |
|---|---|
| Package | The package and its most severe advisory (GHSA and CVE) |
| Vulnerable versions | The versions with unresolved findings present in the registry |
| Findings | How many, the highest severity and the highest CVSS score |
| Upgrade to | Per release line of the vulnerable versions (e.g. 8.x and 10.x) the lowest version that fixes every finding of that line that has a fix: `8.0.4 / 10.0.10` |
| Pulled · 7 d | Downloads of the vulnerable versions in the last 7 days and the distinct clients behind them |
| Assignee | Who fixes it |

The upgrade is chosen from the versions the advisories name as fixes: the
lowest one above every vulnerable version of the line that none of the
line's advisories still affects (an advisory whose ranges reopen later is
checked against all of them). Findings without any fixing release are left
out of the choice and counted as *without a fix*.

**Copy upgrade** copies the command for the package's ecosystem, one line
per release line:

| Format | Command |
|---|---|
| NuGet | `dotnet add package Newtonsoft.Json --version 13.0.1` |
| npm | `npm install lodash@4.17.21` |
| PyPI | `pip install Django==4.2.16` |
| Maven | `mvn versions:use-dep-version -Dincludes=org.apache.logging.log4j:log4j-core -DdepVersion=2.17.1 -DforceVersion=true` |
| Go | `go get golang.org/x/net@v0.33.0` |

Tabs: **In use first** (default: used packages first, then by severity and
pulls; unused ones are dimmed at the end), **All packages** (by severity)
and **Waived** (packages with waived findings). **Assigned to me**, the
repository picker and the search box (package, advisory id, CVE, title)
narrow the table. **Export CSV** downloads the rows of the current tab and
filters.

Click a row to expand it into its findings; click a finding to open it:
severity, CVSS and status, **How to fix** with the upgrade sentence and a
copyable command, **Who pulls it · last 7 days**, the affected ranges, the
publication date, the summary and a link to the full advisory on osv.dev.

### Who pulls a package

Every successful download of a package version is attributed to its client:
the principal (user or service account) for authenticated pulls, otherwise
the client address and the client kind read from the user agent (*NuGet*,
*npm*, *pip*, *Maven*, *Go*, *Docker*, *Browser*, ...). Counts are kept per
version, UTC day and client in `component_pulls`. Like the download counters
they are buffered in memory and written every 15 seconds in one statement;
every node adds its own counts, so the numbers cover the whole cluster and
survive restarts. Rows older than 90 days are deleted. The pull columns start
counting when the server with this feature first runs; *in use* also looks
at the last download time every package already had.

Seeing who pulls a package needs `system:security:read` (or `:write`);
everyone who can read the repository sees the counts.

### Assigning

**Assign** (in the drawer, or the assignee cell of a row for the whole
package) makes an active Hangar user responsible. Assigning a package
assigns all of its unresolved findings, and findings a later scan adds to it
go to the same user. Assigning needs `system:security:write` and is audited
as `finding.assign` / `finding.unassign`. Nobody is notified; the assignee is
shown in the table and the drawer.

## Configuration

```yaml
scanning:
  enabled: true              # HANGAR_SCANNING_ENABLED
  rescan_interval: 24h       # HANGAR_SCANNING_RESCAN_INTERVAL, at least 1h
  in_use_window: 720h        # HANGAR_SCANNING_IN_USE_WINDOW, 24h..2160h: "in use" in the fix plan
  osv:
    url: https://api.osv.dev # HANGAR_SCANNING_OSV_URL
```

- Requests to OSV go through the server's outbound HTTP settings (`http.proxy`
  or `HTTPS_PROXY`, `http.no_proxy`, `http.ca_file`), like proxy repositories.
  Behind a corporate proxy, allow `api.osv.dev:443`.
- **Mirror**: any server that answers `POST /v1/querybatch` and
  `GET /v1/vulns/{id}` like the OSV API works. A mirror on a private address
  must be listed in `http.allow_private_networks`, e.g.
  `HANGAR_HTTP_ALLOW_PRIVATE_NETWORKS=osv.example.internal`.
- `scanning.enabled: false` stops every scan; stored findings stay visible and
  the screen says scanning is off.
- Scan on arrival and quarantine are per repository (**Repository settings →
  Scan on arrival / Quarantine critical**, or `scan_on_arrival` /
  `quarantine_critical` in the repository config).

The task `vulnerability-rescan` is on **Scheduled tasks**; its schedule can be
changed or it can be run now. Its last run reports how many versions were
scanned, the findings and failures.

## Container images

Images in Docker repositories (hosted and proxy) are scanned by a Trivy
server in client/server mode. Their findings are stored with the package
findings and work the same way: severity, CVSS, *fixed in*, status,
waivers, assignment, quarantine, the fix plan, search and the Overview
*Security* card.

| When | What is scanned |
|---|---|
| A tag arrives (push, or a proxy caching it) | That tag, in repositories with **Scan on arrival** |
| Every hour, task `vulnerability-rescan` | Tags never scanned, failed more than 30 minutes ago, or scanned longer than `scanning.rescan_interval` ago (new advisories are found without pulling layers again: the Trivy server keeps what it analysed) |
| On demand | **Rescan**, `POST /api/v1/repositories/{name}:scan`, `POST /api/v1/components/{id}:scan` |

What a scan reports:

- **OS packages** installed by the distribution (Alpine, Debian, Ubuntu,
  Red Hat and its rebuilds, Amazon Linux, SUSE, Wolfi, ...) and **libraries**
  of the applications inside the image (jar files, `node_modules`, Python
  packages, Go and Rust binaries, .NET, PHP, Ruby): everything Trivy's
  vulnerability scanner finds. Secret and misconfiguration scanning are off.
- For each finding: the package and its version, *fixed in*, the
  distribution's verdict when there is no fix (*will not fix*, *fix
  deferred*, *end of life*), the layer that added the package and, for a
  library, the file it was read from (`app/lib/log4j-core-2.14.1.jar`).
- **Severity** is Trivy's (the distribution's rating for OS packages);
  **CVSS** is the score of the source the severity comes from, else NVD,
  else GHSA (CVSS v4.0 first, then v3.x, then v2).
- **Multi-platform images**: every platform manifest of an index is scanned
  by its digest (attestations and other artifacts are skipped) and the
  findings are merged: one finding per advisory, package and file, listing
  the platforms it was found on. `scanner.platforms: default` scans only
  `linux/amd64` (or the first platform when there is none).
- Tags of the same digest are scanned once per scan run.
- Helm charts, signatures and other OCI artifacts stored in a Docker
  repository are not images: they show *Not a container image* (or *A Helm
  chart*) and are skipped.
- Quarantined images stay scannable, so that fixing or waiving releases
  them; the scanner's pulls do not count as downloads.

### Fixing an image

The fix plan has one row per image (`docker:team/app`, across
repositories), with its vulnerable **tags**. There is no version to upgrade
to: an image is fixed by rebuilding it. The *Upgrade to* cell says
**Rebuild · N packages** when N vulnerable packages have a fixed version,
and the row lists them:

- OS packages: rebuild on an updated base image (`FROM alpine:3.20` pulled
  again, or a newer tag), or upgrade them in the Dockerfile. **Copy upgrade**
  copies that line for the image's distribution, e.g.
  `RUN apk upgrade --no-cache libcrypto3 libssl3` (Alpine, Wolfi),
  `RUN apt-get update && apt-get install -y --only-upgrade ...` (Debian,
  Ubuntu), `yum update -y ...` (Red Hat family), `tdnf`, `zypper`.
- Libraries: upgrade them in the application's dependencies (the drawer
  names the file) and rebuild.
- *No fix yet*: the distribution has not shipped a fixed package (or will
  not); a newer base release may not include it, otherwise waive it.

The version shown for a package is the highest version its advisories name
as fixed: Trivy reports no affected ranges for images, so Hangar does not
claim more than "at least this version".

### Design: how Hangar talks to Trivy

Trivy's server holds the vulnerability database but never pulls images:
its RPC (`trivy.scanner.v1.Scanner/Scan`) only reads what a Trivy *client*
analysed and stored in the server's cache (`trivy.cache.v1.Cache`:
`MissingBlobs`, `PutBlob`, `PutArtifact`). The client pulls the image,
unpacks the layers and runs the package analyzers. Three designs were
considered:

| | Design | Verdict |
|---|---|---|
| a | Hangar is the Trivy client: it reads the layers from its own blob store and sends the analysis to the cache API | Rejected: Hangar would re-implement Trivy's analyzers (apk, dpkg, three RPM database formats, jar, npm, Python, Go and Rust binaries, ...) and drift from Trivy's results with every release |
| b | Hangar asks the Trivy server to scan an image reference | Not possible: the server's `Scan` takes an artifact and blob ids already in its cache; it has no pull |
| c | A small scan adapter next to the Trivy server runs the real Trivy client for each request, pulls the image from Hangar by digest and returns the JSON report | **Chosen** |

How (c) works:

1. For a tag, Hangar reads its manifest (for an index, the platform
   manifests) from its own storage.
2. For each manifest it issues a **registry token** for that one
   repository: pull only, valid for 15 minutes (the registry token lifetime),
   signed with the registry token key, not renewable at the token
   endpoint, never stored. The scanner gets no account and no long-lived
   credential.
3. It calls the adapter (`POST /v1/scan`, authenticated with
   `scanner.adapter_token`) with
   `<registry>/<repository>/<image>@sha256:<digest>`; images are always
   scanned by digest. The adapter runs
   `trivy image --server <scanner.trivy_url> --image-src remote --scanners vuln --format json <ref>`
   directly (no shell), passing the token in the environment
   (`TRIVY_REGISTRY_TOKEN`), so it never appears in a command line or log.
4. Trivy pulls the image from Hangar's `/v2/` (`scanner.registry_url`,
   normally the internal address `http://hangar:8080`), stores the layer
   analysis in the server's cache, and the server matches it against the
   database. Layers already analysed are not pulled again.
5. Hangar maps the report to findings.

The Hangar process itself never runs a command: its image has no shell or
Trivy binary. The adapter is the `hangar-scanner` image: the official
Trivy image with the adapter binary, so its Trivy client always matches the
pinned version (keep the server on the same one).

### Setting it up

- **Docker Compose**: `install.sh --with-trivy` (see
  [install-compose.md](../deploy/install-compose.md#image-scanning)). It
  starts the `trivy` and `hangar-scanner` containers (compose profile
  `trivy`), keeps the database in `<data>/trivy/` and generates the adapter
  token.
- **Kubernetes**: `imageScanning.enabled: true` in the Helm chart
  ([install-helm.md](../deploy/install-helm.md#image-scanning)).
- **By hand**: run `trivy server --listen 0.0.0.0:4954 --cache-dir <dir>`
  and the adapter (`ghcr.io/ismoilovdevml/hangar-scanner:<version>`, with
  `HANGAR_SCANNER_TOKEN`), then set:

```yaml
scanner:
  trivy_url: http://trivy:4954             # HANGAR_SCANNER_TRIVY_URL; empty = images are not scanned
  adapter_url: http://hangar-scanner:4955  # HANGAR_SCANNER_ADAPTER_URL
  adapter_token: ""                        # secret: HANGAR_SCANNER_ADAPTER_TOKEN or secrets/scanner.adapter_token
  registry_url: http://hangar:8080         # where the adapter reaches Hangar's /v2/; default server.base_url
  trivy_token: ""                          # secret: the Trivy server's --token, if it has one
  platforms: all                           # all | default
  timeout: 15m                             # one image
  workers: 2                               # images scanned at once
```

`registry_url` must reach the main `/v2/` endpoint (not a connector host or
port). With a plain `http://` URL the adapter allows an insecure registry.

### Corporate proxy and offline databases

The Trivy server downloads its vulnerability database from
`mirror.gcr.io/aquasec/trivy-db` (falling back to `ghcr.io`) on the first
start and every 12 hours after; the adapter downloads the Java index
database (`trivy-java-db`, for jar files) the first time it sees one. Both
honour `HTTPS_PROXY` / `NO_PROXY`:

- Compose: `install.sh --with-trivy --trivy-proxy http://proxy.example.com:3128`
  (stored as `TRIVY_HTTPS_PROXY` in `<data>/.env`; Hangar, `trivy` and
  `hangar-scanner` are always in `NO_PROXY`). Allow `mirror.gcr.io:443`
  (and `ghcr.io:443`, `pkg-containers.githubusercontent.com:443`).
- Helm: `imageScanning.httpsProxy`.

Without internet access:

- **A mirror of the databases** (an OCI registry, e.g. a Hangar Docker proxy
  repository of `ghcr.io`): set `TRIVY_DB_REPOSITORY` on the Trivy server and
  `TRIVY_JAVA_DB_REPOSITORY` on the adapter, in a `compose.override.yaml` next
  to `compose.yaml` (kept on upgrades) or in the chart's
  `imageScanning.trivy.extraEnv` / `imageScanning.scanner.extraEnv`:

  ```yaml
  services:
    trivy:
      environment:
        TRIVY_DB_REPOSITORY: hangar.example.com/ghcr-proxy/aquasecurity/trivy-db:2
    hangar-scanner:
      environment:
        TRIVY_JAVA_DB_REPOSITORY: hangar.example.com/ghcr-proxy/aquasecurity/trivy-java-db:1
  ```

- **A copied database**: download it on a connected machine
  (`trivy image --download-db-only --cache-dir ./cache` and
  `trivy image --download-java-db-only --cache-dir ./cache`), copy
  `cache/db/` into `<data>/trivy/db/` and `cache/java-db/` into
  `<data>/scanner/java-db/` (owner 65532), and set `TRIVY_SKIP_DB_UPDATE=true`
  on the server and `TRIVY_SKIP_JAVA_DB_UPDATE=true` on the adapter. Refresh
  the copy regularly: findings are only as new as the database.

### Troubleshooting images

- ***Image scanning not configured***: `scanner.trivy_url` is empty. With the
  installer, re-run it with `--with-trivy`.
- **The header says *Trivy unreachable***: Hangar cannot read
  `<trivy_url>/version`; check `docker compose logs trivy`. The first start
  downloads the database and can take several minutes.
- **Scans fail with `scan adapter: ... connection refused`**: the
  `hangar-scanner` container is not running or `scanner.adapter_url` is wrong.
- **Scans fail with `... 401 Unauthorized` from the adapter**: the adapter's
  `HANGAR_SCANNER_TOKEN` differs from `scanner.adapter_token`.
- **Scans fail with `unable to ... GET http://hangar:8080/v2/...`**: the
  adapter cannot reach `scanner.registry_url`, or the token audience does
  not match: `server.base_url` must be set (its host is the token's
  audience) and `registry_url` must not be a connector.
- **Scans fail with `scan timed out`**: raise `scanner.timeout` for very
  large images; the first scan of an image pulls every layer.

## Findings, waivers and quarantine

A finding is one advisory on one package version. Statuses:

- **open**: reported by the last scan.
- **waived**: accepted until a date. A waiver needs a reason (3 to 2000
  characters) and an end date at most a year ahead, needs the
  `system:security:write` privilege, and is audited (`finding.waive`,
  `finding.waiver_revoke`). **Waive** on a fix plan row waives every
  finding of the package, or the one picked, with one reason and date. Waived findings do not count as open and do not
  quarantine.
- When a waiver ends the finding is re-opened within a minute (audited as
  `finding.waiver_expired`); the old reason stays visible.
- **fixed**: a later scan no longer reports the advisory for this version
  (for example the advisory was withdrawn or corrected). If it comes back it
  is re-opened with its first-seen date.

**Quarantine critical** (repository setting, off by default): a version with
an open critical finding is quarantined; downloads answer `403` with
*component is quarantined*. The quarantine is lifted when the finding is
fixed or waived, or when the setting is turned off. Quarantines set by hand
are never lifted by the scanner. Every change is audited
(`component.quarantine`, `component.release`) and sent as a live event.

Who sees what: findings follow repository read access. Scanning a version
or a repository on demand needs `system:security:write` or write access to
the repository.

## API

| Call | Purpose |
|---|---|
| `GET /api/v1/findings?severity=critical,high&status=waived&fix_available=true&repository=nuget-proxy&q=CVE-2024-21907` | List with totals by severity |
| `GET /api/v1/findings/{id}` | One finding with the advisory text and references |
| `POST /api/v1/findings/{id}/waiver` `{"reason": "...", "until": "2026-12-31T23:59:59Z"}` | Waive |
| `DELETE /api/v1/findings/{id}/waiver` | Revoke a waiver |
| `GET /api/v1/findings/trend?days=30` | Daily counts of unresolved findings |
| `GET /api/v1/vulnerabilities/fix-plan?tab=in_use&window=7d&assignee=me&q=json` | The fix plan; `format=csv` or `Accept: text/csv` for CSV |
| `POST /api/v1/vulnerabilities/fix-plan/assign` `{"package": "nuget:Newtonsoft.Json", "assignee": "alice"}` | Assign every finding of a package (`null` removes) |
| `POST /api/v1/vulnerabilities/fix-plan/waive` `{"package": "...", "finding_ids": [], "reason": "...", "until": "..."}` | Waive a package's findings (empty list = all) |
| `GET /api/v1/findings/{id}/pullers?window=7d` | Who downloaded the finding's version |
| `PUT` / `DELETE /api/v1/findings/{id}/assignee` `{"assignee": "alice"}` | Assign / unassign one finding |
| `GET /api/v1/vulnerabilities/assignees?q=al` | Users findings can be assigned to |
| `POST` / `GET /api/v1/vulnerabilities/rescan` | Rescan every repository / its progress |
| `GET /api/v1/components/{id}/findings` | Findings and scan state of one version |
| `POST /api/v1/components/{id}:scan` | Scan one version now |
| `POST /api/v1/repositories/{name}:scan` | Queue a scan of a repository |
| `GET /api/v1/scanning/status` | Source, rescan interval, coverage, and image scanning (`images`: Trivy version, database date) |

Examples:

```bash
curl -s -H "Authorization: Bearer $HANGAR_TOKEN" \
  "https://hangar.example.com/api/v1/findings?severity=critical&fix_available=true" | jq '.items[] | {advisory_id, package, installed, fixed_in}'

# What to upgrade, as a spreadsheet
curl -s -H "Authorization: Bearer $HANGAR_TOKEN" -H "Accept: text/csv" \
  "https://hangar.example.com/api/v1/vulnerabilities/fix-plan?tab=all" -o fix-plan.csv
```

## Troubleshooting

- **Everything shows *Scan failed*** with `osv POST /v1/querybatch: dial tcp ...`
  or a timeout: the server cannot reach the advisory database. Check the
  outbound proxy settings, or point `scanning.osv.url` at a mirror.
- **A mirror on a private address is refused** (`blocked by outbound address policy`):
  add it to `http.allow_private_networks`.
- **Nothing is scanned on arrival**: turn on *Scan on arrival* for the
  repository. Everything else is still picked up by the hourly task.
- **The trend counts findings of deleted packages differently**: deleting a
  package deletes its findings, and with them their history.
- **Every row shows 0 pulls right after the upgrade**: pull attribution starts
  when the new server runs; the footer of the table says since when pulls
  are recorded. *In use* already works from the last download times.
- **A row says *Several* as assignee**: its findings are assigned to
  different users (or some to nobody); assign the package to set one.

## Not covered yet

- APT and RPM repositories whose release cannot be detected and is not set,
  and group repositories (their members are scanned).
- Licence policies and signature requirements.
- Secrets and misconfigurations inside images (Trivy's other scanners).
- Findings inside a package's dependencies: Hangar checks the version itself,
  not what it depends on.
