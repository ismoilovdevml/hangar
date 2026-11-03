# Roadmap

Each milestone ends with: green `make test lint e2e`, updated `api/openapi.yaml`,
a short `docs/changelog.md` entry, and a ticked demo checklist.

| # | Milestone | Done when |
|---|---|---|
| M0 | Skeleton | Repo layout, Makefile, docker-compose (postgres, minio, nats, trivy), config loading, migrations, `/healthz`, CI pipeline, ADRs for stack choices. |
| M1 | Core + first formats | Blob stores (fs, s3) with dedup, repositories (hosted/proxy/group), local auth + tokens + RBAC, audit log; Docker, npm, Maven, Raw pass their e2e checklists. |
| M2 | UI v1 | Login, Overview, Repositories, New repository, Package detail, Search, Blob stores match the approved design; live feed via SSE. |
| M3 | Policies & jobs | Scheduler, cleanup policies with preview, compaction, Docker GC, reindex, backup; Tasks and Cleanup screens. |
| M4 | Security | Trivy scanning, findings, waivers, quarantine, cosign verification, licence policy; OIDC, LDAP, SAML; Access & audit and Vulnerabilities screens. |
| M5 | More formats | e2e green for each. |
| M6 | HA & replication | Multi-node with leader lock, local cache, edge replication via NATS, Cluster screen, Helm chart, load test (k6) 5k req/s. |
| M7 | Long tail | e2e green; dark "dusk" theme; Terraform provider. |
