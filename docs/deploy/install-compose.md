# Install with Docker Compose

One command on a Linux host with Docker Engine and the Compose plugin (2.20+):

```bash
curl -fsSL https://github.com/ismoilovdevml/hangar/releases/latest/download/install.sh | sudo bash
```

It asks two questions:

1. **Data directory** (default `/srv/hangar`): everything Hangar stores lives
   there.
2. **Domain** (optional). Empty: Hangar is served at `http://<host-ip>:8080`.
   With a domain it asks whether Hangar gets a certificate itself (Caddy and
   Let's Encrypt on ports 80/443) or sits behind your reverse proxy, which
   terminates TLS.

The installer generates every secret, starts PostgreSQL and Hangar (database
created and migrated automatically), waits until Hangar is ready and prints:

```
  URL:        https://hangar.example.com
  Username:   admin
  Password:   Xk3mP-7qRtz-Hc9vW-2nBfe
              Shown once. Sign in and choose your own password.
  Data:       /srv/hangar
```

plus the commands for logs, upgrade and uninstall. There is no `.env` to fill
in and no environment variable to set.

## Unattended

Every question has a flag; `--yes` (or no terminal) uses flags and defaults.
`--data-dir` is required then.

```bash
# Plain HTTP on the host address
curl -fsSL .../install.sh | sudo bash -s -- --data-dir /srv/hangar --yes

# Automatic HTTPS (DNS points here, ports 80/443 open)
curl -fsSL .../install.sh | sudo bash -s -- --data-dir /srv/hangar \
  --domain hangar.example.com --tls caddy --yes

# Behind a company reverse proxy at 10.0.0.5
curl -fsSL .../install.sh | sudo bash -s -- --data-dir /srv/hangar \
  --domain hangar.example.com --tls proxy --port 8090 --trusted-proxies 10.0.0.5/32 --yes
```

Behind a reverse proxy, pass `--trusted-proxies` so Hangar sees client
addresses (rate limits and audit logs use them), configure the proxy for large
uploads ([reverse-proxy.md](reverse-proxy.md)) and firewall the port so only
the proxy reaches it.

More flags (`--help`): `--version vX.Y.Z`, `--bind`, `--base-url`,
`--acme-email`, `--registry-domain` (a separate host for Docker clients),
`--image`.

## The data directory

| Path | Contents | Owner |
|---|---|---|
| `compose.yaml` | The stack; replaced on upgrade | root |
| `.env` | Installer settings: image, port, URL. No secrets; managed by the installer | root |
| `config/hangar.yaml` | Server settings; yours, never replaced | root |
| `secrets/` | One file per secret, mode 600 | 65532 (Hangar) |
| `postgres/` | PostgreSQL data | 70 (postgres) |
| `blobs/` | Package files | 65532 |
| `nats/` | Job queue state | 65532 |
| `caddy/` | Caddyfile, `sites/`, certificates (with `--tls caddy`) | root |
| `backups/` | Database dump taken before each upgrade (the three newest) | root |

All are bind mounts with SELinux labels (`:Z`, or `:z` for `secrets/`, which
PostgreSQL reads too), so Rocky Linux and RHEL work with SELinux enforcing.
Back up the whole directory; `secrets/security.secret_key` decrypts the
credentials stored in the database.

### Secrets

The server reads `secrets/` through `secrets_dir`: a file named after a
setting's path holds its value. The installer writes
`security.secret_key`, `docker.token_key` and `database.url`, plus
`postgres-password` for PostgreSQL; the server adds
`initial-admin-password` on its first start. To add one, for example an SSO client
secret, create the file and restart:

```bash
cd /srv/hangar
printf '%s' 'the-client-secret' | sudo tee secrets/auth.oidc.client_secret >/dev/null
sudo chown 65532:65532 secrets/auth.oidc.client_secret && sudo chmod 600 secrets/auth.oidc.client_secret
sudo docker compose restart hangar
```

Names without a dot (`postgres-password`, `initial-admin-password`) are not
settings and are skipped; a misspelt dotted name stops the start with an error.
Details: `secrets_dir` in [hangar.example.yaml](../../hangar.example.yaml).

### First administrator

On its first start with an empty database the server creates `admin` with a
one-time password and writes it to `secrets/initial-admin-password` (mode 600,
`bootstrap.initial_password_file`). The installer prints it once; at the first
sign-in you choose your own password, and the file is deleted. A moved
installation keeps its administrators and gets no such password. Lost it
before signing in:
`cd /srv/hangar && sudo docker compose exec hangar hangar admin reset-password --user admin`.
More: [first-admin.md](first-admin.md).

## Settings

- Server settings: `config/hangar.yaml`, then
  `docker compose restart hangar` (run `docker compose` commands in the data
  directory).
- Domain, TLS mode, port, trusted proxies: re-run the installer with the flag;
  everything else is kept.
- Compose changes (extra ports, mounts): `compose.override.yaml` in the data
  directory, which Compose loads automatically. Docker port connectors:
  start from `compose.connectors.example.yaml`.
- Extra Caddy sites (host connectors): `caddy/sites/*.caddy`.
- Resource limits (`HANGAR_MEMORY`, `POSTGRES_SHARED_BUFFERS`, ... in
  `compose.yaml`): add them to `.env`; the installer keeps lines it does not
  manage.

## Upgrade

Run the installer again (the `Upgrade:` line of its summary):

```bash
curl -fsSL https://github.com/ismoilovdevml/hangar/releases/latest/download/install.sh |
  sudo bash -s -- --data-dir /srv/hangar
```

It keeps data, secrets and settings, dumps the database to `backups/`, pulls
the new images and restarts; migrations run on start. `--version vX.Y.Z`
pins a release. More: [upgrade.md](upgrade.md).

## Move an installation from /opt/hangar

Earlier installers used `/opt/hangar` with a `.env` holding the secrets and
the Docker volumes `hangar_postgres-data` and `hangar_hangar-data`. Move it
into a data directory:

```bash
curl -fsSL .../install.sh | sudo bash -s -- --migrate-from /opt/hangar --data-dir /srv/hangar
```

Run interactively, the installer offers this itself when it finds the old
layout. It:

1. checks the old installation (local file storage, bundled PostgreSQL, both
   volumes present) and the free space, and pulls the new images;
2. turns the secrets in `.env` into files in `secrets/` (secret key, Docker
   token key, PostgreSQL password, SSO client secret, LDAP bind password,
   outbound proxy settings) and keeps the domain, URL, port, bind address,
   trusted proxies and resource limits;
3. copies `/opt/hangar/hangar.yaml` (with your `auth.oidc` block and other
   settings) to `config/hangar.yaml`;
4. stops the old stack, copies both volumes into `postgres/`, `blobs/` and
   `nats/`, starts the new stack and waits for `/readyz`.

`/opt/hangar` and the volumes are never changed, so rollback is:

```bash
cd /srv/hangar && sudo docker compose down
cd /opt/hangar && sudo docker compose up -d
```

(changes made after the move are lost). When you no longer need the old
installation: `docker volume rm hangar_postgres-data hangar_hangar-data` and
`rm -rf /opt/hangar`. S3 storage and external databases are not moved
automatically. A `compose.override.yaml` in `/opt/hangar` is not copied;
re-create what you need in the data directory.

## Uninstall

```bash
curl -fsSL .../install.sh | sudo bash -s -- --data-dir /srv/hangar --uninstall
```

removes the containers and keeps the data directory (re-run the installer to
start again). Add `--purge` to delete the data directory too; it asks you to
type the directory's path (`--yes` without a terminal).

## Offline hosts

- `--mirror https://files.example.com/hangar --version vX.Y.Z`: release
  assets from an internal copy (`<URL>/<version>/SHA256SUMS` and
  `hangar-compose.tar.gz`), verified against `SHA256SUMS`.
- `--files-from hangar-compose.tar.gz` (or a directory with its files):
  no download at all.
- Images: `docker load` them first; when a pull fails the installer uses the
  local copies.

Releases before this layout ship no `hangar-compose.tar.gz` with a secrets
directory; install those with their own `install.sh`.

## What the stack does

- **hangar**: read-only root filesystem, all capabilities dropped,
  `no-new-privileges`, health check on `/readyz`, 60 s stop grace period.
  Published on `127.0.0.1` with Caddy, else on all addresses. Applies
  database migrations on start; NATS runs embedded.
- **postgres**: PostgreSQL 16 with data checksums, sized for an 8 GiB host,
  not published on the host.
- **caddy** (`--tls caddy`): certificates, HTTP to HTTPS redirect, HTTP/3,
  streaming without size limits, `/metrics` answered with 404 publicly.
  Hangar trusts `X-Forwarded-*` only from the stack's network and
  `--trusted-proxies`.
- Logs: JSON lines rotated by Docker (5 x 50 MB per container):
  `docker compose logs -f hangar`.
- Monitoring: Prometheus on the same Docker network scrapes
  `hangar:8080/metrics` ([monitoring.md](../operations/monitoring.md)).
