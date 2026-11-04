#!/bin/sh
# Runs the k6 load scenarios against a Hangar server and keeps each summary.
#
#   HANGAR_URL=http://127.0.0.1:8110 HANGAR_ADMIN_PASSWORD=... test/load/run.sh [scenario ...]
#
# Scenarios: raw docker npm maven mixed seeded (default: all; seeded needs
# cmd/hangar-seed data). Summaries are written
# to ${HANGAR_LOAD_OUT:-./load-results}/<scenario>.txt. HANGAR_VUS and
# HANGAR_DURATION set the load shape (default 32 VUs for 30s).
set -eu
cd "$(dirname "$0")"
out=${HANGAR_LOAD_OUT:-./load-results}
mkdir -p "$out"
[ $# -gt 0 ] || set -- raw docker npm maven mixed seeded
status=0
for s in "$@"; do
	echo "== $s"
	if ! k6 run --quiet "$s.js" >"$out/$s.txt" 2>&1; then
		status=1
	fi
	grep -E "✗|level=error|  http_reqs|  http_req_failed|\{ scenario:default" "$out/$s.txt" || true
done
exit $status
