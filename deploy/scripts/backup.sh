#!/usr/bin/env bash
# Back up a Hangar installation: a PostgreSQL dump first, then the blob store.
#
#   BACKUP_DIR=/srv/backups/hangar \
#   HANGAR_DATABASE_URL=postgres://hangar:...@db:5432/hangar \
#   BLOB_SOURCE=fs:/var/lib/hangar/blobs \
#     deploy/scripts/backup.sh
#
# Compose stack (run from the repository root, with the stack's .env loaded):
#
#   set -a; . deploy/compose/.env; set +a
#   HANGAR_COMPOSE_FILE=deploy/compose/compose.yaml BLOB_SOURCE=compose-minio \
#   BACKUP_DIR=/srv/backups/hangar deploy/scripts/backup.sh
#
# Writes BACKUP_DIR/hangar-<UTC timestamp>/ with db.dump (pg_dump custom
# format), blobs/ and MANIFEST, and prints that path. Hangar keeps running.
#
# Why this order is consistent: blobs are content-addressed and never change.
# A blob referenced by the dump is only deleted by blob GC after it has been
# unreferenced for storage.gc.grace_period (default 48h), so copying the blobs
# after the dump, and within that period, captures every blob the dump needs.
# Extra blobs in the copy are harmless (GC removes them after a restore).
#
# Options (environment): see lib.sh for database and blob store access.
#   BACKUP_DIR       parent directory for backups (default ./backups)
#   BACKUP_KEEP      number of backups to keep; older ones are deleted (default 0 = keep all)
#   HANGAR_URL       optional; records the running Hangar version in MANIFEST

set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/scripts/lib.sh
. "$here/lib.sh"

BACKUP_DIR=${BACKUP_DIR:-./backups}
BLOB_SOURCE=${BLOB_SOURCE:-none}
BACKUP_KEEP=${BACKUP_KEEP:-0}
[[ "$BACKUP_KEEP" =~ ^[0-9]+$ ]] || die "BACKUP_KEEP must be a number"

umask 077
mkdir -p "$BACKUP_DIR"
stamp=$(date -u +%Y%m%dT%H%M%SZ)
dest="$BACKUP_DIR/hangar-$stamp"
[[ -e "$dest" ]] && die "$dest already exists"
partial="$dest.partial"
rm -rf "$partial"
mkdir -p "$partial/blobs"
trap 'log "backup failed; partial files left in $partial"' ERR

previous=$(find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d -name 'hangar-*' ! -name '*.partial' | sort | tail -n 1)

log "dumping database ($(db_mode))"
db_dump "$partial/db.dump"
[[ -s "$partial/db.dump" ]] || die "database dump is empty"

log "copying blobs from $BLOB_SOURCE"
blobs_copy "$BLOB_SOURCE" "$partial/blobs" backup "${previous:+$(cd "$previous" && pwd)/blobs}"

version=unknown
if [[ -n "${HANGAR_URL:-}" ]] && command -v curl >/dev/null 2>&1; then
	version=$(curl -fsS --max-time 10 "${HANGAR_URL%/}/api/v1/system/status" |
		sed -n 's/.*"version":"\([^"]*\)".*/\1/p' | head -n 1) || version=unknown
fi
blob_count=$(find "$partial/blobs" -type f | wc -l | tr -d ' ')
cat >"$partial/MANIFEST" <<EOF
created_at=$stamp
hangar_version=${version:-unknown}
db_mode=$(db_mode)
db_dump_sha256=$(sha256 "$partial/db.dump")
blob_source=$BLOB_SOURCE
blob_files=$blob_count
EOF

mv "$partial" "$dest"
trap - ERR
log "backup complete: $dest ($blob_count blob files)"

if ((BACKUP_KEEP > 0)); then
	backups=$(find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d -name 'hangar-*' ! -name '*.partial' | sort)
	total=$(printf '%s\n' "$backups" | grep -c .)
	printf '%s\n' "$backups" | awk -v n=$((total - BACKUP_KEEP)) 'NR <= n' | while read -r old; do
		log "removing old backup $old"
		rm -rf "$old"
	done
fi
echo "$dest"
