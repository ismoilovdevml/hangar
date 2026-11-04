# Vulnerability scanning

Hangar checks every npm, Maven, PyPI, NuGet and Go package version it stores
against the public [OSV](https://osv.dev) advisory database (or a mirror of
it) and lists what it finds on the **Vulnerabilities** screen, on the package
page and in search (`security:findings`, `cve:CVE-2024-21907`).

Docker images are **not** scanned yet. They show *Image scanning not
configured*, never *Clean*. Raw files have no advisory source and show *No
advisory source*.

## How it works

| When | What is scanned |
|---|---|
| A package arrives (hosted publish or a proxy caching it) | That version, in repositories with **Scan on arrival** (`scan_on_arrival: true`) |
| Every hour, task `vulnerability-rescan` | Versions never scanned, versions whose last scan failed (after 30 minutes), and versions whose last scan is older than `scanning.rescan_interval` (default 24 h), in every repository, so new advisories are picked up |
| On demand | One version: `POST /api/v1/components/{id}:scan` or **Scan now** in the finding drawer. A repository: `POST /api/v1/repositories/{name}:scan` or **Scan *repo* now** on the Vulnerabilities screen |

For each version Hangar asks OSV `POST /v1/querybatch` (up to 1000 versions
per request, result pages followed) and then `GET /v1/vulns/{id}` for each
advisory it has not seen at that modification time. Requests are rate
limited (10 per second) and retried with exponential backoff on network
errors, `429` (honouring `Retry-After`) and `5xx`.

A scan that fails keeps the findings of the previous scan and records the
error; the package shows *Scan failed* and the hourly task retries it.

Package names are sent the way advisories name them: npm `@scope/name`, Maven
`groupId:artifactId`, NuGet the original-case package id, PyPI the project
name, Go the module path.

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

## Configuration

```yaml
scanning:
  enabled: true              # HANGAR_SCANNING_ENABLED
  rescan_interval: 24h       # HANGAR_SCANNING_RESCAN_INTERVAL, at least 1h
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
  `finding.waiver_revoke`). Waived findings do not count as open and do not
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
| `GET /api/v1/findings/trend?days=30` | Daily counts for the trend chart |
| `GET /api/v1/components/{id}/findings` | Findings and scan state of one version |
| `POST /api/v1/components/{id}:scan` | Scan one version now |
| `POST /api/v1/repositories/{name}:scan` | Queue a scan of a repository |
| `GET /api/v1/scanning/status` | Source, rescan interval and coverage |

Example:

```bash
curl -s -H "Authorization: Bearer $HANGAR_TOKEN" \
  "https://hangar.example.com/api/v1/findings?severity=critical&fix_available=true" | jq '.items[] | {advisory_id, package, installed, fixed_in}'
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

## Not covered yet

- Container images (and the `scanner.trivy_url` setting, which is reserved).
- Licence policies and signature requirements.
- Findings inside a package's dependencies: Hangar checks the version itself,
  not what it depends on.
