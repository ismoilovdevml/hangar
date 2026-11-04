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

Only one process may migrate at a time: migrations take no cluster-wide lock.
With several nodes, run `hangar migrate up` once (the Helm chart does this in
a hook Job) and keep `migrate_on_start: false` on the nodes.

Old nodes on a newer schema: a release can only be rolled out node by node if
its changelog entry says its migrations are compatible with the previous
version. Otherwise stop all nodes, migrate, and start the new version.

`hangar migrate down` reverts one migration. Prefer restoring the backup
taken before the upgrade: down migrations can drop data that the newer
version wrote.

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
flight get `server.shutdown_timeout` to finish.

To go back, restore the dump taken before the upgrade and re-run the
installer with the previous `--version`. Installations in `/opt/hangar` from
earlier installers move to a data directory first:
[install-compose.md](install-compose.md#move-an-installation-from-opthangar).

## Helm

```bash
helm upgrade hangar deploy/helm/hangar -n hangar -f values-prod.yaml \
  --set image.tag=<new version> --wait
kubectl -n hangar rollout status deploy/hangar
helm test hangar -n hangar
```

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

Several nodes (`migrate_on_start: false`):

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
