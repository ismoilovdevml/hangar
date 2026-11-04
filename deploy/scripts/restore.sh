#!/usr/bin/env bash
# Restore a backup made by backup.sh: blobs first, then the database.
#
#   HANGAR_DATABASE_URL=postgres://hangar:...@db:5432/hangar \
#   BLOB_SOURCE=fs:/var/lib/hangar/blobs \
#     deploy/scripts/restore.sh /srv/backups/hangar/hangar-20261104T020000Z
#
# Stop every Hangar process first (docker compose stop hangar, or scale the
# Deployment to 0): the database is replaced in one transaction and running
# nodes would see it change underneath them. Then start Hangar again; if the
# backup is older than the running version, apply migrations first
# (`hangar migrate up`, the Helm migration hook, or migrate_on_start).
#
# The target database should be a fresh, empty database created for Hangar
# (same owner as before). Objects in the dump replace existing ones
# (pg_restore --clean --if-exists), but objects that exist only in the target
# are left alone. Blobs are only added, never deleted.
#
# Options (environment): see lib.sh for database and blob store access.
#   RESTORE_YES=1    do not ask for confirmation

set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/scripts/lib.sh
. "$here/lib.sh"

[[ $# -eq 1 ]] || die "usage: restore.sh BACKUP_PATH"
src=${1%/}
[[ -f "$src/MANIFEST" && -f "$src/db.dump" ]] || die "$src is not a backup made by backup.sh"
BLOB_SOURCE=${BLOB_SOURCE:-none}

expected=$(sed -n 's/^db_dump_sha256=//p' "$src/MANIFEST")
actual=$(sha256 "$src/db.dump")
[[ "$expected" == "$actual" ]] || die "db.dump checksum mismatch (MANIFEST $expected, file $actual)"

log "backup: $src"
sed 's/^/  /' "$src/MANIFEST" >&2
log "target database: $(db_mode); blob store: $BLOB_SOURCE"
if [[ "${RESTORE_YES:-}" != 1 ]]; then
	read -r -p "Hangar must be stopped. Replace the database contents now? [y/N] " answer
	[[ "$answer" == y || "$answer" == Y ]] || die "aborted"
fi

log "restoring blobs"
blobs_copy "$BLOB_SOURCE" "$src/blobs" restore

log "restoring database"
db_restore "$src/db.dump"

log "restore complete; start Hangar (apply migrations first if it is newer than the backup)"
