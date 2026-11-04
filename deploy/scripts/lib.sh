#!/usr/bin/env bash
# Shared helpers for backup.sh and restore.sh. Not meant to be run directly.
#
# Database access (one of):
#   HANGAR_DATABASE_URL=postgres://...      pg_dump / pg_restore on this host
#   HANGAR_COMPOSE_FILE=deploy/compose/compose.yaml
#                                           run them inside the compose postgres
#                                           service (POSTGRES_USER / POSTGRES_DB
#                                           from the environment, default hangar)
#
# Blob store (BLOB_SOURCE):
#   fs:/var/lib/hangar/blobs     filesystem store root (storage.fs.path); rsync
#   s3://bucket[/prefix]         S3 store; aws CLI (AWS_* credentials,
#                                S3_ENDPOINT_URL for MinIO and other endpoints)
#   compose-minio                the compose stack's MinIO; mc in the minio-init
#                                image, credentials from MINIO_ROOT_USER /
#                                MINIO_ROOT_PASSWORD, bucket HANGAR_STORAGE_S3_BUCKET
#   none                         database only

set -euo pipefail

log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }
die() { log "error: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

compose() {
	need docker
	docker compose -f "${HANGAR_COMPOSE_FILE}" "$@"
}

db_mode() {
	if [[ -n "${HANGAR_COMPOSE_FILE:-}" ]]; then
		echo compose
	elif [[ -n "${HANGAR_DATABASE_URL:-}" ]]; then
		echo url
	else
		die "set HANGAR_DATABASE_URL or HANGAR_COMPOSE_FILE"
	fi
}

# db_dump FILE: custom-format dump (compressed, restorable selectively).
db_dump() {
	local out=$1
	case "$(db_mode)" in
	url)
		need pg_dump
		pg_dump --format=custom --compress=6 --no-password --file="$out" "${HANGAR_DATABASE_URL}"
		;;
	compose)
		compose exec -T postgres pg_dump --format=custom --compress=6 \
			-U "${POSTGRES_USER:-hangar}" -d "${POSTGRES_DB:-hangar}" >"$out"
		;;
	esac
}

# db_restore FILE: replaces every Hangar object in the target database.
db_restore() {
	local in=$1
	local args=(--clean --if-exists --no-owner --exit-on-error --single-transaction)
	case "$(db_mode)" in
	url)
		need pg_restore
		pg_restore "${args[@]}" --no-password --dbname="${HANGAR_DATABASE_URL}" "$in"
		;;
	compose)
		compose exec -T postgres pg_restore "${args[@]}" \
			-U "${POSTGRES_USER:-hangar}" -d "${POSTGRES_DB:-hangar}" <"$in"
		;;
	esac
}

# blobs_copy SRC DST: copies blobs one way without deleting anything at the
# destination (blobs are immutable and content-addressed). SRC and DST are
# a BLOB_SOURCE value and a local directory, in either order; the direction
# is given by $3 (backup|restore). $4 is an optional previous backup's blob
# directory used for hard links (fs only), so unchanged blobs cost no space.
blobs_copy() {
	local source=$1 dir=$2 direction=$3 link_dest=${4:-}
	case "$source" in
	none) return 0 ;;
	fs:*)
		need rsync
		local root=${source#fs:}
		[[ -d "$root" ]] || die "blob store root $root does not exist"
		if [[ "$direction" == backup ]]; then
			local extra=()
			[[ -n "$link_dest" && -d "$link_dest" ]] && extra=(--link-dest="$link_dest")
			# tmp/ holds in-flight uploads only.
			rsync -a --exclude=/tmp/ "${extra[@]}" "$root"/ "$dir"/
		else
			rsync -a --ignore-existing "$dir"/ "$root"/
		fi
		;;
	s3://*)
		need aws
		local endpoint=()
		[[ -n "${S3_ENDPOINT_URL:-}" ]] && endpoint=(--endpoint-url "${S3_ENDPOINT_URL}")
		if [[ "$direction" == backup ]]; then
			aws "${endpoint[@]}" s3 sync --only-show-errors "$source" "$dir"
		else
			aws "${endpoint[@]}" s3 sync --only-show-errors "$dir" "$source"
		fi
		;;
	compose-minio)
		: "${MINIO_ROOT_USER:?set MINIO_ROOT_USER (compose .env)}"
		: "${MINIO_ROOT_PASSWORD:?set MINIO_ROOT_PASSWORD (compose .env)}"
		local bucket=${HANGAR_STORAGE_S3_BUCKET:-hangar}
		local abs
		abs=$(cd "$dir" && pwd)
		local from="src/${bucket}" to=/backup
		[[ "$direction" == restore ]] && from=/backup to="src/${bucket}"
		# MC_HOST_<alias> configures the alias without writing a config file.
		compose run --rm --no-deps -T \
			-e "MC_HOST_src=http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@minio:9000" \
			-v "${abs}:/backup" --entrypoint mc minio-init \
			mirror --quiet "$from" "$to" >/dev/null
		;;
	*) die "BLOB_SOURCE $source: want fs:/path, s3://bucket[/prefix], compose-minio or none" ;;
	esac
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}
