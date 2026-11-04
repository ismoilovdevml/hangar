# Hangar

A self-hosted package registry. One server hosts, proxies and groups
**Docker/OCI** (including Helm charts and cosign signatures), **npm**,
**Maven**, **PyPI**, **NuGet** and **raw** artifacts, with tokens, role-based
access and an audit log. It is a single Go binary backed by PostgreSQL, with
blobs on a filesystem or S3.

Hangar is written from the public protocol specifications of each format. CI
systems such as GitLab CI, GitHub Actions, Gitea and Jenkins are clients that
authenticate with tokens; Hangar does not depend on any of them.

## Quick start

```bash
export HANGAR_DATABASE_URL='postgres://hangar@localhost:5432/hangar?sslmode=disable'
export HANGAR_BOOTSTRAP_ADMIN_USER=admin HANGAR_BOOTSTRAP_ADMIN_PASSWORD='<password>'
go run ./cmd/hangar serve --migrate
curl http://localhost:8080/readyz
```

Or run everything in containers with
`docker compose -f deploy/docker-compose.yml up -d --build`. The
[getting started guide](docs/guides/getting-started.md) goes on from there to
a token, a repository, and a first upload and download.

## Using it

| Client | Guide |
|---|---|
| docker, buildx, helm (OCI), cosign | [docs/guides/docker.md](docs/guides/docker.md) |
| npm, pnpm, yarn | [docs/guides/npm.md](docs/guides/npm.md) |
| Maven, Gradle | [docs/guides/maven.md](docs/guides/maven.md) |
| pip, uv, poetry, twine | [docs/guides/pypi.md](docs/guides/pypi.md) |
| dotnet, NuGet | [docs/guides/nuget.md](docs/guides/nuget.md) |
| curl, wget | [docs/guides/raw.md](docs/guides/raw.md) |
| GitLab CI, GitHub Actions, Jenkins, Gitea | [docs/guides/ci.md](docs/guides/ci.md) |
| REST API with tokens | [docs/guides/api.md](docs/guides/api.md), [api/openapi.yaml](api/openapi.yaml) |
| Users, roles and tokens | [docs/guides/access.md](docs/guides/access.md) |

## Operating it

- Configuration: [hangar.example.yaml](hangar.example.yaml). Every key can also
  be set with a `HANGAR_*` environment variable, for example `database.url` is
  `HANGAR_DATABASE_URL`.
- Health and metrics: `/healthz`, `/readyz`, `/metrics`;
  [monitoring](docs/operations/monitoring.md) and [runbooks](docs/operations/runbooks.md).
- Capacity, load tests and tuning: [docs/operations/performance.md](docs/operations/performance.md).
- Security: [docs/security.md](docs/security.md); report vulnerabilities as
  described in [SECURITY.md](SECURITY.md).
- Design: [architecture](docs/ARCHITECTURE.md), [formats](docs/FORMATS.md),
  [decision records](docs/adr/), [roadmap](docs/ROADMAP.md),
  [changelog](docs/changelog.md).

## Development

Requires Go (see `go.mod`) and PostgreSQL 16 or newer. Node 22 or newer is
needed only for the web UI in `web/`.

```bash
make test    # Go and web unit tests; set HANGAR_TEST_DATABASE_URL for database tests
make lint    # golangci-lint, eslint, tsc
make e2e     # real-client acceptance tests against HANGAR_E2E_URL
make help    # everything else
```
