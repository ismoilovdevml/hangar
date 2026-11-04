# Backup and restore

A Hangar installation is three things; back up all of them:

| What | Where | How |
|---|---|---|
| Metadata | PostgreSQL | `pg_dump` (custom format) |
| Artifact bytes | the blob store: S3 bucket or `storage.fs.path` | add-only copy (blobs never change) |
| Keys | `HANGAR_SECRET_KEY` (and previous keys during a rotation), `HANGAR_DOCKER_TOKEN_KEY` | your secret store, separately from the data |

Without `HANGAR_SECRET_KEY` a restored database still serves every artifact,
but credentials it stores (proxy upstream passwords and tokens, S3 keys of
extra blob stores) cannot be decrypted and must be entered again. Embedded
NATS state is not needed: it only holds queued jobs, which the scheduler
recreates.

## Why dump first, then copy blobs

Blobs are content-addressed and immutable. A blob that the dump references
is deleted by blob GC only after it has been unreferenced for
`storage.gc.grace_period` (default 48 h). So a blob copy that **starts after
the dump and finishes within the grace period** contains every blob the dump
needs. Blobs added after the dump are harmless extras; GC removes them after
a restore. Hangar keeps running during the backup.

## The scripts

`deploy/scripts/backup.sh` and `deploy/scripts/restore.sh` implement this.
Configuration is by environment variables:

| Variable | Meaning |
|---|---|
| `HANGAR_DATABASE_URL` | Run `pg_dump`/`pg_restore` on this host against this URL (client 16+). |
| `HANGAR_COMPOSE_FILE` | Instead: run them inside the compose stack's `postgres` service (`POSTGRES_USER`, `POSTGRES_DB`). |
| `BLOB_SOURCE` | `fs:/var/lib/hangar/blobs` (rsync), `s3://bucket/prefix` (aws CLI; `S3_ENDPOINT_URL` for MinIO and others), `compose-minio` (the stack's MinIO via `mc`; `MINIO_ROOT_USER`, `MINIO_ROOT_PASSWORD`, `HANGAR_STORAGE_S3_BUCKET`), or `none`. |
| `BACKUP_DIR` | Parent directory (default `./backups`). |
| `BACKUP_KEEP` | Keep this many backups, delete older ones (default 0: keep all). |
| `HANGAR_URL` | Optional: records the running version in `MANIFEST`. |
| `RESTORE_YES=1` | `restore.sh` without the confirmation prompt. |

Each run writes `BACKUP_DIR/hangar-<UTC time>/` with `db.dump`, `blobs/` and
`MANIFEST` (time, version, checksum of the dump, blob count) and prints the
path. A run that fails leaves `*.partial` and exits non-zero. For filesystem
stores, unchanged blobs are hard-linked to the previous backup, so daily
backups cost only the new bytes.

### Compose stack

For an installation made with `install.sh` (data directory `/srv/hangar`):

```bash
HANGAR_COMPOSE_FILE=/srv/hangar/compose.yaml BLOB_SOURCE=fs:/srv/hangar/blobs \
BACKUP_DIR=/srv/backups/hangar BACKUP_KEEP=14 HANGAR_URL=http://127.0.0.1:8080 \
  deploy/scripts/backup.sh
```

Also keep a copy of `/srv/hangar/secrets/` (it does not change after the
install): `security.secret_key` decrypts credentials stored in the database.
A stopped stack can simply be backed up as the whole data directory.

### Kubernetes / external services

```bash
HANGAR_DATABASE_URL='postgres://hangar:...@pg.example:5432/hangar?sslmode=require' \
BLOB_SOURCE=s3://hangar-blobs AWS_PROFILE=hangar-backup \
BACKUP_DIR=/srv/backups/hangar deploy/scripts/backup.sh
```

For large S3 stores, a copy to the backup host is often impractical. Use the
storage's own protection instead: bucket versioning with a lifecycle rule
that expires noncurrent versions after longer than the grace period, or
replication to a second bucket or region. Then back up only the database
(`BLOB_SOURCE=none`) and keep the dumps at least as long as the versions.

### Schedule

A systemd timer (or cron) on the backup host:

```ini
# /etc/systemd/system/hangar-backup.service
[Service]
Type=oneshot
EnvironmentFile=/etc/hangar/backup.env
ExecStart=/opt/hangar/deploy/scripts/backup.sh

# /etc/systemd/system/hangar-backup.timer
[Timer]
OnCalendar=*-*-* 02:30:00
Persistent=true
[Install]
WantedBy=timers.target
```

Copy finished backups off the host, and test a restore regularly.

## Restore

1. Stop every Hangar process (`docker compose stop hangar`,
   `kubectl scale deploy/hangar --replicas=0`, `systemctl stop hangar`).
2. Provide an empty database owned by the Hangar role (or the existing one,
   whose Hangar objects are replaced).
3. Run the restore with the same variables as the backup, pointing at the
   target:

   ```bash
   HANGAR_COMPOSE_FILE=/srv/hangar/compose.yaml BLOB_SOURCE=fs:/srv/hangar/blobs \
     deploy/scripts/restore.sh /srv/backups/hangar/hangar-20261104T023000Z
   ```

   It verifies the dump's checksum, copies the blobs back without deleting
   anything, then restores the database in one transaction
   (`pg_restore --clean --if-exists --single-transaction`).
4. Set the same `HANGAR_SECRET_KEY` and `HANGAR_DOCKER_TOKEN_KEY` as before.
5. Start Hangar. If the running version is newer than the backup, migrations
   apply first (`migrate_on_start`, the Helm hook, or `hangar migrate up`).
6. Check: `/readyz` is ok, a known artifact downloads, the audit log shows
   the latest events from before the backup.

The procedure was tested end to end for both modes: a filesystem store with
a database URL, and the compose stack with MinIO (backup, `docker compose
down -v`, restore, start, and the uploaded artifact downloaded with the same
SHA-256).

## Point-in-time recovery

`pg_dump` gives one consistent point per run. For a smaller recovery point,
use PostgreSQL's continuous archiving (WAL archiving with pgBackRest, Barman,
WAL-G, or your managed service's PITR) and keep blob GC's grace period longer
than the distance between the recovery target and the blob copy.
