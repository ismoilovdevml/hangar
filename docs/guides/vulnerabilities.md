# Vulnerability scanning

Hangar checks every npm, Maven, PyPI, NuGet, Go and Cargo package version it
stores, and the APT and RPM packages of repositories whose distribution
release is known, against the public [OSV](https://osv.dev) advisory
database (or a mirror of it) and lists what it finds on the **Vulnerabilities** screen as a fix plan
(what to upgrade, and whether builds still pull the vulnerable versions), on
the package page and in search (`security:findings`, `cve:CVE-2024-21907`).

Docker images are **not** scanned yet. They show *Image scanning not
configured*, never *Clean*. Raw files have no advisory source and show *No
advisory source*, and so do APT and RPM packages until their repository
names a [distribution release](#apt-and-rpm-distribution-releases).

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
| `GET /api/v1/scanning/status` | Source, rescan interval and coverage |

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

- Container images (and the `scanner.trivy_url` setting, which is reserved).
- APT and RPM repositories whose release cannot be detected and is not set,
  and group repositories (their members are scanned).
- Licence policies and signature requirements.
- Findings inside a package's dependencies: Hangar checks the version itself,
  not what it depends on.
