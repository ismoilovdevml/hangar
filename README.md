# Hangar

**A self-hosted, enterprise package registry.** One server hosts, proxies and groups
Docker/OCI, npm, Maven, PyPI, Go, Helm, NuGet, Cargo, APT and raw artifacts — with
vulnerability scanning, RBAC and SSO, cleanup policies, HA clustering and replication.

Hangar is written from scratch against the public protocol specifications of each format.
It runs fully standalone: CI systems (GitLab CI, GitHub Actions, Gitea, Jenkins, …) are
clients that authenticate with tokens, never runtime dependencies.

> Status: early development. See [docs/ROADMAP.md](docs/ROADMAP.md).

## Documentation

- [Product specification](docs/PRODUCT_SPEC.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Formats and acceptance tests](docs/FORMATS.md)
- [Design system](docs/DESIGN_SYSTEM.md)
- [Roadmap](docs/ROADMAP.md)

## Getting started

Requirements: Go (see `go.mod`), PostgreSQL 16+, and Docker for the bundled
dependencies and the end-to-end tests. Node 22+ only to work on the web UI.

Run everything in containers:

```bash
docker compose -f deploy/docker-compose.yml up -d --build
curl http://localhost:8080/readyz
```

Or run Hangar from source against containerised dependencies:

```bash
make dev-deps        # postgres, minio, nats, trivy
make run             # build and run `hangar serve --migrate`
make dev             # same, with live reload and the web dev server
```

Without Docker, point Hangar at any PostgreSQL; it embeds NATS and stores blobs
on disk by default:

```bash
export HANGAR_DATABASE_URL=postgres://user@localhost:5432/hangar?sslmode=disable
go run ./cmd/hangar serve --migrate
```

Configuration: copy `hangar.example.yaml` and pass `--config`, or set
`HANGAR_*` environment variables (`database.url` -> `HANGAR_DATABASE_URL`).

Development:

```bash
make test            # Go and web unit tests (HANGAR_TEST_DATABASE_URL enables DB tests)
make lint            # golangci-lint, eslint, tsc
make e2e             # real-client acceptance tests (HANGAR_E2E_URL, needs Docker)
make help            # everything else
```

Design decisions are recorded in [docs/adr](docs/adr/).

## Stack

Go (single `hangar` binary) · PostgreSQL · S3/MinIO or filesystem blob storage ·
NATS JetStream · Trivy · React + TypeScript web UI.
