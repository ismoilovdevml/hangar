# Roadmap

Each milestone ends with: green `make test lint e2e`, updated `api/openapi.yaml`,
a short `docs/changelog.md` entry, and a ticked demo checklist.

Status as of v0.1.6 and the Unreleased section of the [changelog](changelog.md):
**done** (shipped and covered by tests), **partial** (shipped with the gaps
named) or **planned** (not implemented; API operations for it are marked
`x-status: planned` in `api/openapi.yaml` and answer 501).

| # | Milestone | Done when | Status |
|---|---|---|---|
| M0 | Skeleton | Repo layout, Makefile, docker-compose (postgres, minio, nats, trivy), config loading, migrations, `/healthz`, CI pipeline, ADRs for stack choices. | Partial: everything exists; the CI workflow is switched off while development is fast, so the release workflow runs the full test suite itself before it publishes. |
| M1 | Core + first formats | Blob stores (fs, s3) with dedup, repositories (hosted/proxy/group), local auth + tokens + RBAC, audit log; Docker, npm, Maven, Raw pass their e2e checklists. | Done. The S3 driver's conformance tests run against a fake S3 server everywhere and against MinIO in the CI workflow. |
| M2 | UI v1 | Login, Overview, Repositories, New repository, Package detail, Search, Blob stores match the approved design; live feed via SSE. | Done. The Overview has no recent-publications list, so it fits one screen; live updates arrive over `GET /api/v1/events`. |
| M3 | Policies & jobs | Scheduler, cleanup policies with preview, compaction, Docker GC, reindex, backup; Tasks and Cleanup screens. | Partial: reindex is planned; backups are the `deploy/scripts/backup.sh` script, not a scheduled task. |
| M4 | Security | Trivy scanning, findings, waivers, quarantine, cosign verification, licence policy; OIDC, LDAP, SAML; Access & audit and Vulnerabilities screens. | Partial: cosign verification, licence policy and SAML are planned. |
| M5 | More formats | e2e green for each. | Done: Go, Helm (classic and OCI), NuGet, PyPI, Cargo, APT and RPM, 11 formats in all. |
| M6 | HA & replication | Multi-node with leader lock, local cache, edge replication via NATS, Cluster screen, Helm chart, load test (k6) 5k req/s. | Partial: multi-node with a leader lock and the Helm chart are done; the on-disk blob cache, edge replication and the Cluster screen are planned; the measured load is about 1 100 req/s on one node (mixed traffic, a laptop), see [performance](operations/performance.md). |
| M7 | Long tail | e2e green; dark "dusk" theme; Terraform provider. | Planned: the dusk colour tokens exist but are not wired to a switch or to `prefers-color-scheme`; no Terraform provider. |

## Item by item

| Item | Status | Notes |
|---|---|---|
| Multi-node (HA) | Done | Stateless nodes, PostgreSQL advisory-lock leader, NATS job queue; [high availability](deploy/high-availability.md). |
| Edge replication | Planned | `/api/v1/replication/targets` answers 501; the Cluster screen is a placeholder. |
| On-disk blob cache per node | Planned | Nodes keep only in-memory caches today. |
| Image scanning (Trivy) | Done | Optional scan adapter; [vulnerabilities](guides/vulnerabilities.md). |
| Package scanning (OSV) | Done | npm, Maven, PyPI, NuGet, Go, Cargo, APT and RPM. |
| Quarantine | Done | Docker images and scanned packages. |
| Cosign signatures | Partial | Signatures are stored and served like any other OCI artifact, so `cosign sign` and `cosign verify` work against Hangar (an e2e test covers it when cosign is installed); Hangar itself does not verify signatures or require them by policy. |
| Licence policy | Planned | |
| OIDC and LDAP / Active Directory | Done | [SSO guide](guides/sso.md). |
| SAML | Planned | The sign-in page reports it as not configured. |
| Component SBOM download | Planned | `GET /components/{id}/sbom` answers 501. |
| Component dependents | Planned | `GET /components/{id}/dependents` answers 501. |
| Moving repositories between blob stores | Planned | `POST /blob-stores/{name}/move-repositories` answers 501. |
| Search reindex task | Planned | `reindex` is a reserved task type. |
| Scheduled database backup | Planned | Use `deploy/scripts/backup.sh` ([backup and restore](deploy/backup-restore.md)). |
| Config as code (`hangar apply -f`, `hangar export`) | Planned | The New repository screen shows the settings as YAML for review and copying; there is no CLI that applies it. |
| Terraform provider | Planned | |
| Dark theme | Planned | Tokens only (`web/src/styles/tokens.css`). |
