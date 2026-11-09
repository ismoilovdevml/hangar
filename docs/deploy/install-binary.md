# Install the binary with systemd

For VMs and bare metal without containers. Hangar is one static binary; the
unit file is `deploy/systemd/hangar.service`.

## Build or download

Build a release binary with the web UI embedded (Go and Node 22 required):

```bash
make build-embed VERSION=1.0.0      # bin/hangar for this machine
# or for another platform:
make web-build
find internal/webui/dist -mindepth 1 ! -name .gitkeep -delete
cp -R web/dist/. internal/webui/dist/
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -tags webui \
  -ldflags "-s -w -X github.com/ismoilovdevml/hangar/internal/version.Version=1.0.0" \
  -o hangar-linux-amd64 ./cmd/hangar
```

Or copy it out of the container image:

```bash
id=$(docker create ghcr.io/ismoilovdevml/hangar:<version>)
docker cp "$id":/usr/local/bin/hangar ./hangar && docker rm "$id"
```

`hangar version` prints the version, commit and build date.

## Install

```bash
sudo install -m 0755 hangar /usr/local/bin/hangar
sudo useradd --system --home-dir /var/lib/hangar --shell /usr/sbin/nologin hangar
sudo install -d -m 0750 -o root -g hangar /etc/hangar
```

`/etc/hangar/hangar.yaml` (non-secret settings; start from
[`hangar.example.yaml`](../../hangar.example.yaml)):

```yaml
server:
  addr: "127.0.0.1:8080"            # the reverse proxy on this host connects here
  base_url: https://hangar.example.com
  trusted_proxies: [127.0.0.1]
storage:
  kind: fs
  fs:
    path: /var/lib/hangar/blobs     # or kind: s3 with storage.s3.*
nats:
  embedded: true
  data_dir: /var/lib/hangar/nats
database:
  migrate_on_start: true            # safe on several nodes too (advisory lock)
```

`/etc/hangar/hangar.env` (secrets, `VAR=value` per line, no quotes needed):

```bash
HANGAR_DATABASE_URL=postgres://hangar:PASSWORD@127.0.0.1:5432/hangar?sslmode=disable
HANGAR_SECRET_KEY=<openssl rand -base64 32>
HANGAR_DOCKER_TOKEN_KEY=<openssl rand -base64 48>
HANGAR_BOOTSTRAP_ADMIN_USER=admin
HANGAR_BOOTSTRAP_ADMIN_PASSWORD=<at least 12 characters>
```

```bash
sudo chown root:hangar /etc/hangar/hangar.yaml /etc/hangar/hangar.env
sudo chmod 0640 /etc/hangar/hangar.yaml /etc/hangar/hangar.env
sudo install -m 0644 deploy/systemd/hangar.service /etc/systemd/system/hangar.service
sudo systemctl daemon-reload
sudo systemctl enable --now hangar
systemctl status hangar
curl -fsS http://127.0.0.1:8080/readyz
journalctl -u hangar -f
```

Then put a reverse proxy with TLS in front ([reverse-proxy.md](reverse-proxy.md)),
sign in, and remove the bootstrap lines from `hangar.env`
([first-admin.md](first-admin.md)).

## The unit

- Runs as the unprivileged `hangar` user with `/var/lib/hangar` as its only
  writable directory (`StateDirectory`), a private `/tmp`, no capabilities,
  and a system call filter. `systemd-analyze security hangar.service` rates
  it 1.4 ("OK").
- `ExecStart=/usr/local/bin/hangar serve`, configuration from
  `HANGAR_CONFIG=/etc/hangar/hangar.yaml` and `EnvironmentFile=/etc/hangar/hangar.env`.
- On stop, Hangar fails readiness first, keeps serving for
  `server.drain_delay` (0 s by default; set it when a load balancer polls
  `/readyz`) and lets in-flight uploads finish within
  `server.shutdown_timeout` (30 s); `TimeoutStopSec=60s` leaves room for
  both while `drain_delay + shutdown_timeout` stays below 60 s.
- `Restart=on-failure`: a failed start (for example PostgreSQL unreachable) is
  retried every 5 s.
- Blobs on NFS or another mount outside `/var/lib/hangar`: add the path to
  the unit with `sudo systemctl edit hangar`:

  ```ini
  [Service]
  ReadWritePaths=/mnt/hangar-blobs
  ```

## Several nodes

Run the same binary and configuration on each node with:

- `storage.kind: s3` (or a shared filesystem mounted at the same path),
- `nats.embedded: false`, `nats.url` pointing at a 3-node NATS cluster
  with JetStream, and `nats.replicas: 3`,
- the same `HANGAR_SECRET_KEY` and `HANGAR_DOCKER_TOKEN_KEY` everywhere,
- migrations once per upgrade with `hangar migrate up`, or
  `database.migrate_on_start: true` on every node (an advisory lock lets one
  node migrate while the others wait; [upgrade.md](upgrade.md)),
- a load balancer in front that checks `/readyz` (no sticky sessions needed),
  and `server.drain_delay` covering its health check interval x unhealthy
  threshold.

See [high-availability.md](high-availability.md).
