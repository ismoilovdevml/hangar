# Upgrades

## Before every upgrade

1. Read the release's entry in [docs/changelog.md](../changelog.md), in
   particular new required settings and migrations.
2. Take a backup ([backup-restore.md](backup-restore.md)).
3. Pin the new version explicitly (image tag, chart `image.tag`, binary).

## How migrations work

The schema is versioned with goose migrations embedded in the binary.
`hangar migrate status` lists them, `hangar migrate up` applies pending ones.
`hangar serve` refuses to start on a schema that is behind
(`database schema is out of date`) unless it migrates itself
(`database.migrate_on_start: true` or `serve --migrate`).

Migrations run under a PostgreSQL advisory lock (per schema): when several
processes migrate at once (nodes starting together with `migrate_on_start`,
or `hangar migrate up` next to starting nodes), one applies the migrations
and the others log `waiting for the migration lock`, wait for it and then find
nothing pending. A node without `migrate_on_start` also waits for a running
migration instead of failing. The lock needs a session-level connection:
not PgBouncer in transaction pooling mode.

With several nodes, prefer running `hangar migrate up` once before the
rollout (the Helm chart does this in a hook Job): a failed migration then
stops the upgrade before any node runs the new version.
`migrate_on_start: true` on every node is safe too.

Old nodes on a newer schema: a release can only be rolled out node by node if
its changelog entry says its migrations are compatible with the previous
version. Otherwise stop all nodes, migrate, and start the new version.

`hangar migrate down` reverts one migration. Prefer restoring the backup
taken before the upgrade: down migrations can drop data that the newer
version wrote.

## Upgrading to 0.2.0

v0.2.0 removes NATS. Events between nodes go through PostgreSQL
(LISTEN/NOTIFY) and the job queue is the `jobs` table
([ADR 0011](../adr/0011-postgresql-events-and-jobs.md)).

**Before the upgrade**

- PostgreSQL connections: every node opens one more long-lived session (the
  event listener). Allow `nodes x (database.max_conns + 17)` connections. A
  PgBouncer in front of Hangar must use session pooling for its database;
  in transaction pooling mode the listener cannot work and the nodes stay
  unready ([high-availability.md](high-availability.md#pgbouncer)).
- Several nodes: each node reads the System logs of the others over HTTP at
  `server.node_url` (by default the address it reaches the database from,
  with the port of `server.addr`). Open that port between the nodes. The
  Helm chart sets the URL to the pod IP and allows the traffic in its
  NetworkPolicy.
- Jobs waiting in NATS JetStream are not carried over. Let the queue drain
  first if you want none lost (`hangar_job_queue_depth` at 0 on the Jobs
  dashboard). Scheduled jobs (blob GC, audit partitions, cleanup, token
  purge) are enqueued again on their next schedule either way.
- Take a database backup, as before every upgrade. It is also your way back
  (see the secrets note below).

**Upgrade all nodes together.** v0.1 nodes use NATS and v0.2 nodes use
PostgreSQL, so in a mixed cluster they do not share events (cache
invalidation) or jobs. Stop or replace every node in one go:

- Compose (one node): re-run the installer as below.
- Helm: upgrade from the OCI chart, which is published from this release on:

  ```bash
  helm upgrade hangar oci://ghcr.io/ismoilovdevml/charts/hangar --version 0.2.0 \
    -n hangar -f values-prod.yaml --wait
  ```

  The rolling update keeps old and new pods side by side for a few minutes,
  during which the old pods may serve stale cached answers and jobs they
  enqueue are lost. To avoid that window, at the cost of a short outage,
  add `--set updateStrategy.type=Recreate --set updateStrategy.rollingUpdate=null`
  to this one upgrade. If your values come from a checkout of the
  repository (`deploy/helm/hangar`), switch to the OCI chart now.
- Binary and systemd: stop every node, run `hangar migrate up` once, then
  start every node on the new binary.

**Remove the `nats` settings.** `nats.*` in `hangar.yaml` and
`HANGAR_NATS_*` variables are ignored; the server logs one warning at start
(`deprecated configuration: nats.* (HANGAR_NATS_*) is ignored`). Helm values
under `nats:` are ignored too (the schema still accepts them, and the release
notes print a reminder).

**Remove NATS once every node runs 0.2.0:**

- Helm with the bundled NATS: the upgrade deletes its StatefulSet, Services
  and Secret but not its volumes:
  `kubectl -n hangar delete pvc -l app.kubernetes.io/instance=hangar,app.kubernetes.io/component=nats`.
- External NATS: decommission it, or remove Hangar's streams
  (`HANGAR_JOBS`) and user if other applications still use it.
- Compose: `<data>/nats` is no longer mounted; the installer leaves it in
  place and prints `rm -rf <data>/nats` for when you no longer need a
  rollback to v0.1.x.
- Binary: the directory of the former `nats.data_dir`.

**Metrics.** `hangar_nats_connected` is now `hangar_event_listener_connected`
and `hangar_nats_reconnects_total` is now
`hangar_event_listener_reconnects_total`; `hangar_job_queue_dead` is new. The
bundled dashboards and alerts use the new names; update your own.

**Stored secrets (`hgs2`).** On its first start v0.2.0 re-seals the
credentials stored in the database (proxy upstream passwords, blob store keys)
in the `hgs2` format, bound to their row and field
([ADR 0010](../adr/0010-context-bound-sealed-secrets.md)). v0.1.x cannot read
`hgs2`. To go back, restore the backup taken before the upgrade (it still
holds `hgs1`), and bring NATS back for v0.1.x. If you keep the database
instead, stop every v0.2 node and run `hangar admin rotate-secrets --downgrade`
with the v0.2.0 binary before you start v0.1.x: a v0.1.x server that finds
`hgs2` values takes them for plaintext and seals them again, and those
credentials stop working until they are entered again.

## Compose

Re-run the installer with the data directory (add `--version vX.Y.Z` to pin
a release):

```bash
curl -fsSL https://github.com/ismoilovdevml/hangar/releases/latest/download/install.sh |
  sudo bash -s -- --data-dir /srv/hangar
```

It dumps the database to `<data>/backups/` (the three newest are kept), pulls
the new images, restarts the stack (migrations run on start) and waits for
`/readyz`. Data, secrets, `config/hangar.yaml` and the installer settings are
kept. Downtime is the restart (a few seconds, plus migrations). Uploads in
flight get `server.shutdown_timeout` to finish. If the dump fails, nothing
is restarted; fix the cause and re-run the same command, which takes the
dump again before it upgrades.

Without `--version` the installer asks the GitHub API for the latest release
(60 requests an hour per address without a token; set `GITHUB_TOKEN` in the
environment to lift that) and otherwise follows the redirect of the
`releases/latest` page.

To go back, restore the dump taken before the upgrade and re-run the
installer with the previous `--version`. Installations in `/opt/hangar` from
earlier installers move to a data directory first:
[install-compose.md](install-compose.md#move-an-installation-from-opthangar).

## Helm

```bash
helm upgrade hangar oci://ghcr.io/ismoilovdevml/charts/hangar --version <new version> \
  -n hangar -f values-prod.yaml --wait
kubectl -n hangar rollout status deploy/hangar
helm test hangar -n hangar
```

The chart's version is the release version and it installs that release's
images. If your values pin `image.tag` or `image.digest`, change them in the
same upgrade. Releases before v0.2.0 have no OCI chart: upgrade to them from
a checkout of the tag (`helm upgrade hangar deploy/helm/hangar ...`).

The pre-upgrade hook Job runs `hangar migrate up` with the new image before
any pod is replaced; if it fails, the upgrade stops and the old pods keep
running (`kubectl -n hangar logs job/hangar-migrate`). The rolling update then
replaces pods one at a time (`maxUnavailable: 0`), each pod leaving the
Service before it stops.

Rollback: `helm rollback hangar <revision>` restores the previous pods but not
the schema. Roll back only if the newer migrations are compatible with the
older version (changelog); otherwise restore the backup.

## Binary and systemd

Single node:

```bash
sudo install -m 0755 hangar-<new version> /usr/local/bin/hangar
sudo systemctl restart hangar     # migrate_on_start: true
journalctl -u hangar -n 50
```

Several nodes (with `migrate_on_start: true` on every node, restarting them
in turn is enough; otherwise migrate first):

```bash
# on one node, with the new binary and the same configuration
sudo systemd-run --uid=hangar --wait --pipe --quiet \
  -p EnvironmentFile=/etc/hangar/hangar.env -E HANGAR_CONFIG=/etc/hangar/hangar.yaml \
  /usr/local/bin/hangar migrate up
# then on each node in turn
sudo install -m 0755 hangar-<new version> /usr/local/bin/hangar && sudo systemctl restart hangar
```

## After the upgrade

- `/readyz` on every node, `hangar version` (or `GET /api/v1/system/status`)
  shows the new version.
- Dashboards: error rate and latency back to normal.
- New settings from the changelog are in place (this release adds, for
  example, `HANGAR_SECRET_KEY`; see [docs/security.md](../security.md)).
