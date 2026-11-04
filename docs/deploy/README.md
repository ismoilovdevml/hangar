# Deploying Hangar

How to run Hangar in production: what it needs, three ways to install it, and
how to operate it afterwards.

| Page | Read it for |
|---|---|
| [Requirements and sizing](requirements.md) | PostgreSQL, blob storage, NATS, CPU, memory, disk, network |
| [Install with Docker Compose](install-compose.md) | One host: Hangar, PostgreSQL, MinIO and Caddy with automatic TLS |
| [Install with Helm](install-helm.md) | Kubernetes, two or more replicas, external PostgreSQL and S3 |
| [Install the binary with systemd](install-binary.md) | VMs and bare metal without containers |
| [TLS and reverse proxies](reverse-proxy.md) | Caddy, Traefik, Kubernetes ingress; large uploads; a separate registry host |
| [Corporate HTTP proxy](corporate-proxy.md) | Reaching Maven Central, npmjs, Docker Hub through a proxy, private CAs |
| [First administrator](first-admin.md) | Creating the first account and the first token |
| [Backup and restore](backup-restore.md) | `deploy/scripts/backup.sh` and `restore.sh`, what must be kept |
| [Upgrades](upgrade.md) | Migrations and the upgrade procedure for each install method |
| [High availability](high-availability.md) | Several nodes: what is shared, what fails over, known limits |
| [Troubleshooting](troubleshooting.md) | Symptoms, causes and fixes |

Related: [security](../security.md) (secret key, rate limits, hardening),
[monitoring](../operations/monitoring.md) (metrics, logs, traces, dashboards,
alerts), [runbooks](../operations/runbooks.md), [performance](../operations/performance.md),
and the user guides in [docs/guides](../guides/README.md).

## Which install method?

| | Compose | Helm | Binary + systemd |
|---|---|---|---|
| Nodes | 1 | 1 to many | 1 to many |
| PostgreSQL | bundled or external | external | external |
| Blob storage | bundled MinIO, external S3 or a volume | S3, or a ReadWriteMany volume | local disk, NFS or S3 |
| TLS | Caddy (automatic) | your ingress controller / Gateway | your reverse proxy |
| Event bus (NATS) | embedded | bundled 3-node cluster, external, or embedded (1 replica) | embedded, or an external cluster |
| Upgrades | `docker compose pull && up -d` | `helm upgrade` (migration hook) | replace binary, `hangar migrate up`, restart |

## Container image

The examples use `ghcr.io/ismoilovdevml/hangar:<version>`. Until release
images are published there, build the image from a release tag and push it
to a registry your hosts can pull from, then set `HANGAR_IMAGE` (compose) or
`image.repository`/`image.tag` (Helm):

```bash
git checkout <version>
make docker IMAGE=registry.internal.example/hangar:<version>
docker push registry.internal.example/hangar:<version>
```

The build is reproducible: the same commit, `VERSION`, `COMMIT` and
`SOURCE_DATE_EPOCH` (all set by `make docker`) produce identical binaries.
Multi-architecture images: `docker buildx build --platform
linux/amd64,linux/arm64 --build-arg VERSION=... --push -t <image> .`.

## Configuration in one paragraph

Hangar reads `hangar.yaml` (`--config` or `HANGAR_CONFIG`) and `HANGAR_*`
environment variables, which win over the file. Every key, its default and
its environment variable is documented in
[`hangar.example.yaml`](../../hangar.example.yaml). Unknown keys stop the
server at start. Secrets belong in the environment (or Kubernetes Secrets),
never in the file: `HANGAR_DATABASE_URL`, `HANGAR_SECRET_KEY`,
`HANGAR_DOCKER_TOKEN_KEY`, `HANGAR_STORAGE_S3_SECRET_ACCESS_KEY`,
`HANGAR_HTTP_PROXY_PASSWORD`, `HANGAR_BOOTSTRAP_ADMIN_PASSWORD`.

The settings every production install sets:

| Setting | Why |
|---|---|
| `server.base_url` | The public URL. Every link Hangar generates (npm tarballs, the Docker token realm) uses it. |
| `server.trusted_proxies` | The reverse proxy's address. Without it every client appears to come from the proxy, and all of them share one rate-limit bucket. |
| `HANGAR_SECRET_KEY` | Encrypts credentials stored in the database. Back it up with the database. |
| `HANGAR_DOCKER_TOKEN_KEY` | Signs registry tokens. Must be identical on every node and stable across restarts. |
| `database.url` | PostgreSQL 16 or newer. |
| `storage.*` | Where blobs live: S3 or a filesystem path. |
