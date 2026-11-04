# Install with Docker Compose

One host runs Hangar, PostgreSQL 16, MinIO and Caddy. Caddy obtains TLS
certificates automatically and serves two host names: the UI host
(`hangar.example.com`: UI, API, every package format) and a registry host for
Docker clients (`registry.example.com`). Files: `deploy/compose/`.

| File | Purpose |
|---|---|
| `compose.yaml` | The stack. Optional services are enabled with `COMPOSE_PROFILES`. |
| `.env.example` | Every setting and secret; copy to `.env`. |
| `hangar.yaml` | Non-secret tuning, mounted read-only into the container. |
| `Caddyfile` | TLS, the two hosts, streaming, `/metrics` blocked publicly. |
| `compose.connectors.example.yaml` | Publishes Docker port connectors. |

## Prerequisites

- Docker Engine 24+ with the Compose plugin v2.24+.
- DNS A/AAAA records for both host names pointing at the host, and ports 80
  and 443 (TCP, plus 443/UDP for HTTP/3) reachable from the Internet for
  Let's Encrypt. Behind a firewall without inbound Internet access, see
  "Internal certificates" below.
- Sizing: [requirements.md](requirements.md). The defaults suit a host with
  8 GiB RAM and 4 vCPU.

## Steps

```bash
git clone https://github.com/ismoilovdevml/hangar.git
cd hangar/deploy/compose
cp .env.example .env
chmod 600 .env
```

Edit `.env`:

1. Host names: `HANGAR_DOMAIN`, `HANGAR_REGISTRY_DOMAIN`, `ACME_EMAIL`, and
   `HANGAR_BASE_URL=https://<HANGAR_DOMAIN>`.
2. Secrets, each from `openssl rand -hex 24` unless noted (URL-safe values
   only, they end up inside connection URLs):
   - `POSTGRES_PASSWORD`
   - `MINIO_ROOT_PASSWORD`, `HANGAR_STORAGE_S3_SECRET_ACCESS_KEY`
   - `HANGAR_DOCKER_TOKEN_KEY`: `openssl rand -base64 48`
   - `HANGAR_SECRET_KEY`: `openssl rand -base64 32`
   - `HANGAR_BOOTSTRAP_ADMIN_PASSWORD`: at least 12 characters
3. Image: pin `HANGAR_IMAGE` to a release tag (`ghcr.io/ismoilovdevml/hangar:<version>`)
   rather than `latest`, so that upgrades are deliberate.

Start and check:

```bash
docker compose up -d
docker compose ps            # every service "healthy"; minio-init "exited (0)"
docker compose logs hangar | tail
curl -fsS https://hangar.example.com/readyz
```

Open `https://hangar.example.com`, sign in as the bootstrap administrator,
change the password, then remove `HANGAR_BOOTSTRAP_ADMIN_USER` and
`HANGAR_BOOTSTRAP_ADMIN_PASSWORD` from `.env` and run `docker compose up -d`
again. More in [first-admin.md](first-admin.md).

Store `.env` (it holds `HANGAR_SECRET_KEY`) in your password manager or
secret store: a database backup is only fully usable with that key.

## What the stack does

- **hangar**: read-only root filesystem, all capabilities dropped,
  `no-new-privileges`, healthcheck on `/readyz` via the built-in
  `hangar-healthcheck`, 60 s stop grace period (uploads in flight get
  `server.shutdown_timeout`). Published on `127.0.0.1:8080` only; Caddy is
  the public entry. Applies database migrations on start
  (`HANGAR_DATABASE_MIGRATE_ON_START=true`). NATS runs embedded with its state
  in the `hangar-data` volume.
- **postgres**: PostgreSQL 16 with data checksums and settings for an 8 GiB
  host (`POSTGRES_SHARED_BUFFERS`, `POSTGRES_EFFECTIVE_CACHE_SIZE`, ... in
  `.env`). Not published on the host.
- **minio** and **minio-init**: MinIO refuses to start without root
  credentials. `minio-init` creates the bucket and a user limited to it, so
  the root credentials never reach Hangar. The console is published on
  `127.0.0.1:9001`; reach it through an SSH tunnel.
- **caddy**: certificates for both hosts, HTTP to HTTPS redirect, HTTP/3,
  streaming in both directions without size limits, `/metrics` answered with
  404 on the public host. Hangar trusts `X-Forwarded-*` only from the stack's
  Docker network (`HANGAR_NETWORK_SUBNET`, `HANGAR_TRUSTED_PROXIES`).
- Logs: JSON lines, rotated by Docker (5 x 50 MB per container).
- Resource limits: `HANGAR_CPUS`/`HANGAR_MEMORY`, `POSTGRES_CPUS`/`POSTGRES_MEMORY`,
  `MINIO_CPUS`/`MINIO_MEMORY`, `CADDY_CPUS`/`CADDY_MEMORY`.

## Variants

**External PostgreSQL.** Remove `postgres` from `COMPOSE_PROFILES` and set
`HANGAR_DATABASE_URL=postgres://hangar:PASSWORD@db.internal:5432/hangar?sslmode=verify-full`
(percent-encode special characters in the password).

**AWS S3 or another S3 service.** Remove `minio` from `COMPOSE_PROFILES` and
set `HANGAR_STORAGE_S3_ENDPOINT=` (empty for AWS, or the service URL),
`HANGAR_STORAGE_S3_REGION`, `HANGAR_STORAGE_S3_BUCKET`,
`HANGAR_STORAGE_S3_USE_PATH_STYLE=false` for AWS, and the access key pair.

**Filesystem blobs.** `HANGAR_STORAGE_KIND=fs` stores blobs in the
`hangar-data` volume (`/var/lib/hangar/blobs`). Remove `minio` from
`COMPOSE_PROFILES`.

**Your own reverse proxy.** Remove `caddy` from `COMPOSE_PROFILES`, publish
Hangar where your proxy reaches it (`HANGAR_BIND_ADDR`, `HANGAR_HTTP_PORT`)
and set `HANGAR_TRUSTED_PROXIES` to the proxy's address. Settings for large
uploads: [reverse-proxy.md](reverse-proxy.md).

**Corporate outbound proxy.** `HANGAR_HTTP_PROXY`, `HANGAR_HTTP_NO_PROXY` and
the credentials in `.env`; see [corporate-proxy.md](corporate-proxy.md).

**Docker port connectors.** A Docker repository with
`"connector": {"http_port": 5001}` listens on its own port inside the
container. Publish it with the override file:

```bash
cp compose.connectors.example.yaml compose.connectors.yaml   # list your ports
docker compose -f compose.yaml -f compose.connectors.yaml up -d
```

Port connectors are plain HTTP; prefer host connectors
(`"connector": {"host": "cr.example.com"}`) behind Caddy with TLS. A host
connector needs its own site block in the `Caddyfile`, written like the
`{$HANGAR_REGISTRY_DOMAIN}` block.

**Internal certificates.** Without inbound Internet access, give Caddy your
own certificates: add `tls /etc/caddy/certs/hangar.crt /etc/caddy/certs/hangar.key`
to each site block and mount the files, or use `tls internal` for a private
CA that clients must trust.

**Monitoring.** Prometheus on the same Docker network scrapes
`hangar:8080/metrics`; dashboards and alert rules are in
`deploy/observability/` ([monitoring.md](../operations/monitoring.md)).

## Validate changes

From the repository root, `make compose-config` checks `compose.yaml` and the
connector override against `.env.example`. After editing `.env`, run
`docker compose config --quiet` in `deploy/compose/`: it fails with a clear
message when a required secret is missing.

## Day 2

- Backups: [backup-restore.md](backup-restore.md) (compose mode of
  `deploy/scripts/backup.sh`).
- Upgrades: [upgrade.md](upgrade.md).
- Logs: `docker compose logs -f hangar`; set `log.level: debug` in
  `hangar.yaml` temporarily for more detail.
