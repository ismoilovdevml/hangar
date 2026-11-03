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

## Stack

Go (single `hangar` binary) · PostgreSQL · S3/MinIO or filesystem blob storage ·
NATS JetStream · Trivy · React + TypeScript web UI.
