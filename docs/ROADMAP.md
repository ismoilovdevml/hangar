# Roadmap

Each milestone ends with: green `make test lint e2e`, updated `api/openapi.yaml`,
a short `docs/changelog.md` entry, and a ticked demo checklist.

Status as of v0.1.9 (2026-11-20) and the work in progress for v0.2.0 (see
the [changelog](changelog.md)): **done** (shipped and covered by tests),
**partial** (shipped with the gaps named), **in progress** (being built for
the next release, not shipped) or **planned** (not implemented; API
operations for it are marked `x-status: planned` in `api/openapi.yaml` and
answer 501).

## Releases so far

| Release | Date | Main content |
|---|---|---|
| v0.1.0 | 2026-11-04 | First release: multi-arch image on ghcr.io, binaries with `SHA256SUMS`, the compose installer; web UI with packages, search and blob stores. |
| v0.1.1 | 2026-11-04 | OIDC and LDAP / Active Directory sign-in. |
| v0.1.2 – v0.1.5 | 2026-11-04 – 2026-11-05 | Cluster-wide Overview traffic, System status, README images, the public OpenAPI document and automation guide. |
| v0.1.6 | 2026-11-09 | Go modules, Helm chart repositories, Cargo, APT and RPM: 11 formats. |
| v0.1.7 | 2026-11-09 | Routing rules against dependency confusion; cluster operations (job stream replicas, migration lock, `/metrics` authentication); `npm audit` answered for real. |
| v0.1.8 | 2026-11-15 | Local times across the web UI and many UI fixes; releases run the test suite before publishing. |
| v0.1.9 | 2026-11-20 | Security and reliability: SSRF through repository proxies, sealed secret replay, NuGet proxy cache poisoning, archive bombs; the chart defaults to the release's images. |

## v0.2.0 (in progress)

| Item | Status | Notes |
|---|---|---|
| Remove NATS | In progress | Hangar stops depending on NATS JetStream for jobs and events. |
| Webhooks | In progress | Outgoing notifications of repository events. |
| CI OIDC token exchange | In progress | CI jobs (GitHub Actions, GitLab CI) exchange their OIDC identity token for a short-lived Hangar token instead of storing one. |
| Config as code (`hangar apply`) | In progress | Apply configuration from YAML files with a CLI. |
| `hgs2` sealed secrets | In progress | The next version of the sealed secret format (`hgs1.` today). |
| Search with `pg_trgm` | In progress | Search backed by PostgreSQL trigram (`pg_trgm`) indexes. |
| Release supply chain | In progress | cosign signatures, SPDX SBOMs and SLSA provenance for the images, the chart and the release files; the Helm chart on `oci://ghcr.io/ismoilovdevml/charts/hangar`; Trivy, govulncheck and npm audit gate the release ([verify a release](deploy/verify-release.md)). |

Notes for v0.2.0 items describe the goal; the [changelog](changelog.md)
describes what actually ships.

| # | Milestone | Done when | Status |
|---|---|---|---|
| M0 | Skeleton | Repo layout, Makefile, docker-compose (postgres, minio, nats, trivy), config loading, migrations, `/healthz`, CI pipeline, ADRs for stack choices. | Partial: everything exists; the CI workflow is switched off while development is fast; the release workflow runs the full test suite before it publishes (unless the `RELEASE_SKIP_TESTS` repository variable is `true`). |
| M1 | Core + first formats | Blob stores (fs, s3) with dedup, repositories (hosted/proxy/group), local auth + tokens + RBAC, audit log; Docker, npm, Maven, Raw pass their e2e checklists. | Done. The S3 driver's conformance tests run against a fake S3 server everywhere and against MinIO in the CI workflow. |
| M2 | UI v1 | Login, Overview, Repositories, New repository, Package detail, Search, Blob stores match the approved design; live feed via SSE. | Done. The Overview has no recent-publications list, so it fits one screen; live updates arrive over `GET /api/v1/events`. |
| M3 | Policies & jobs | Scheduler, cleanup policies with preview, compaction, Docker GC, reindex, backup; Tasks and Cleanup screens. | Partial: reindex is planned; backups are the `deploy/scripts/backup.sh` script, not a scheduled task. |
| M4 | Security | Trivy scanning, findings, waivers, quarantine, cosign verification, licence policy; OIDC, LDAP, SAML; Access, Audit log and Vulnerabilities screens. | Partial: cosign verification, licence policy and SAML are planned. |
| M5 | More formats | e2e green for each. | Done: Go, Helm (classic and OCI), NuGet, PyPI, Cargo, APT and RPM, 11 formats in all. |
| M6 | HA & replication | Multi-node with leader lock, local cache, edge replication via NATS, Cluster screen, Helm chart, load test (k6) 5k req/s. | Partial: multi-node with a leader lock and the Helm chart are done; the Cluster screen lists the nodes; the on-disk blob cache and edge replication are planned; the measured load is about 1 100 req/s on one node (mixed traffic, a laptop), see [performance](operations/performance.md). |
| M7 | Long tail | e2e green; dark "dusk" theme; Terraform provider. | Planned: the dusk colour tokens exist but are not wired to a switch or to `prefers-color-scheme`; no Terraform provider. |

## Item by item

| Item | Status | Notes |
|---|---|---|
| Multi-node (HA) | Done | Stateless nodes, PostgreSQL advisory-lock leader, NATS job queue; [high availability](deploy/high-availability.md). |
| Edge replication | Planned | `/api/v1/replication/targets` answers 501; the web UI's replication placeholder was removed in v0.1.9. |
| On-disk blob cache per node | Planned | Nodes keep only in-memory caches today. |
| Image scanning (Trivy) | Done | Optional scan adapter; [vulnerabilities](guides/vulnerabilities.md). |
| Package scanning (OSV) | Done | npm, Maven, PyPI, NuGet, Go, Cargo, APT and RPM. |
| Quarantine | Done | Docker images and scanned packages. |
| Routing rules (dependency confusion) | Done | Since v0.1.7; [routing rules](guides/routing-rules.md). |
| Cosign signatures | Partial | Signatures are stored and served like any other OCI artifact, so `cosign sign` and `cosign verify` work against Hangar (an e2e test covers it when cosign is installed); Hangar itself does not verify signatures or require them by policy. |
| Licence policy | Planned | |
| OIDC and LDAP / Active Directory | Done | [SSO guide](guides/sso.md). |
| SAML | Planned | The sign-in page reports it as not configured. |
| Component SBOM download | Planned | `GET /components/{id}/sbom` answers 501. |
| Component dependents | Planned | `GET /components/{id}/dependents` answers 501. |
| Moving repositories between blob stores | Planned | `POST /blob-stores/{name}/move-repositories` answers 501. |
| Search reindex task | Planned | `reindex` is a reserved task type. |
| Scheduled database backup | Planned | Use `deploy/scripts/backup.sh` ([backup and restore](deploy/backup-restore.md)). |
| Config as code (`hangar apply -f`, `hangar export`) | In progress (v0.2.0) | Up to v0.1.9 the New repository screen shows the settings as YAML for review and copying, and no CLI applies it. |
| Terraform provider | Planned | |
| Dark theme | Planned | Tokens only (`web/src/styles/tokens.css`). |
