#!/usr/bin/env bash
# Real-client acceptance tests with everything in containers: PostgreSQL,
# MinIO, a Hangar image and the client image (test/e2e/runner/Dockerfile).
# The suite runs with HANGAR_E2E_STRICT=1, so a missing client or an
# unreachable Docker daemon fails instead of skipping. The S3 blob store
# conformance test runs against the MinIO container in the same go.
#
#   test/e2e/run-docker.sh                       # build both images, run everything
#   test/e2e/run-docker.sh -run 'TestHelm|TestRPM'  # extra arguments go to go test
#
# Needs a Linux host with Docker (containers share the host network, so the
# Docker daemon, the APT/RPM containers and the clients all reach Hangar at
# http://localhost:$E2E_PORT) and internet access for the proxy tests. CI
# runs this script (.github/workflows/ci.yml, job e2e).
#
#   HANGAR_IMAGE     Hangar image to test (default: built from this checkout as hangar:e2e)
#   RUNNER_IMAGE     client image (default: built as hangar-e2e-runner:dev)
#   E2E_PORT         Hangar port (default 18080); PostgreSQL uses 55432, MinIO 59000
#   E2E_WORKDIR      scratch directory shared with the containers
#                    (default ${RUNNER_TEMP:-/tmp}/hangar-e2e)
#   HANGAR_E2E_ALLOW_MISSING  clients that may be absent (see test/e2e/doc.go)
#   KEEP=1           leave the containers running afterwards
#
# TestImageScan (needs a Trivy server) and TestLDAP (needs directories) have
# their own runners: HANGAR_E2E_IMAGE_SCAN_STACK=1 and test/e2e/ldap/run.sh.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
port=${E2E_PORT:-18080}
pg_port=55432
minio_port=59000
work=${E2E_WORKDIR:-${RUNNER_TEMP:-/tmp}/hangar-e2e}
hangar_image=${HANGAR_IMAGE:-}
runner_image=${RUNNER_IMAGE:-}
postgres_image=postgres:16.15-alpine
minio_image=pgsty/minio:RELEASE.2026-08-04T00-00-00Z

pg_name=hangar-e2e-postgres
minio_name=hangar-e2e-minio
hangar_name=hangar-e2e

if [[ "$(uname -s)" != Linux ]]; then
  echo "run-docker.sh needs Linux: the containers share the host network" >&2
  exit 2
fi

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    echo "--- hangar logs (tail)"
    docker logs "$hangar_name" 2>&1 | tail -n 200 || true
  fi
  if [[ "${KEEP:-0}" != 1 ]]; then
    docker rm -f "$hangar_name" "$minio_name" "$pg_name" >/dev/null 2>&1 || true
  fi
  exit "$rc"
}
trap cleanup EXIT

wait_for() { # what, command...
  local what=$1; shift
  for _ in $(seq 1 90); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  echo "timed out waiting for $what" >&2
  return 1
}

if [[ -z "$hangar_image" ]]; then
  hangar_image=hangar:e2e
  docker build -t "$hangar_image" --build-arg VERSION=e2e "$root"
fi
if [[ -z "$runner_image" ]]; then
  runner_image=hangar-e2e-runner:dev
  docker build -t "$runner_image" "$here/runner"
fi

mkdir -p "$work/go-cache" "$work/go-mod" "$work/tmp"
docker rm -f "$hangar_name" "$minio_name" "$pg_name" >/dev/null 2>&1 || true

docker run -d --name "$pg_name" --network host \
  -e POSTGRES_USER=hangar -e POSTGRES_PASSWORD=hangar -e POSTGRES_DB=hangar -e PGPORT="$pg_port" \
  "$postgres_image" >/dev/null
docker run -d --name "$minio_name" --network host \
  -e MINIO_ROOT_USER=hangar -e MINIO_ROOT_PASSWORD=hangar-secret \
  "$minio_image" server /data --address "127.0.0.1:$minio_port" --console-address "127.0.0.1:$((minio_port + 1))" >/dev/null
wait_for postgres docker exec "$pg_name" pg_isready -U hangar -d hangar -p "$pg_port"
wait_for minio curl -fsS "http://127.0.0.1:$minio_port/minio/health/ready"

admin_password="e2e-$(openssl rand -hex 12)"
docker run -d --name "$hangar_name" --network host \
  -e HANGAR_SERVER_ADDR="127.0.0.1:$port" \
  -e HANGAR_SERVER_BASE_URL="http://localhost:$port" \
  -e HANGAR_DATABASE_URL="postgres://hangar:hangar@127.0.0.1:$pg_port/hangar?sslmode=disable" \
  -e HANGAR_DATABASE_MIGRATE_ON_START=true \
  -e HANGAR_SECRET_KEY="$(openssl rand -base64 32)" \
  -e HANGAR_DOCKER_TOKEN_KEY="$(openssl rand -base64 48)" \
  -e HANGAR_HTTP_ALLOW_PRIVATE_NETWORKS="127.0.0.0/8,::1/128,localhost" \
  -e HANGAR_BOOTSTRAP_ADMIN_USER=admin \
  -e HANGAR_BOOTSTRAP_ADMIN_PASSWORD="$admin_password" \
  -e HANGAR_BOOTSTRAP_REQUIRE_PASSWORD_CHANGE=false \
  "$hangar_image" >/dev/null
wait_for hangar curl -fsS "http://127.0.0.1:$port/readyz"

token=$(docker exec "$hangar_name" /usr/local/bin/hangar admin token create \
  --user admin --name e2e --scopes 'repo:*:admin,system:*:admin' --expires 1d | tr -d '[:space:]')
[[ "$token" == hgr_* ]] || { echo "could not create an access token" >&2; exit 1; }

# The repository is mounted at its own path and the scratch directory is
# TMPDIR: paths the tests hand to `docker run -v` then mean the same on the
# host's daemon as inside the client container.
docker run --rm --network host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$root:$root" -w "$root" \
  -v "$work:$work" \
  -e TMPDIR="$work/tmp" -e GOCACHE="$work/go-cache" -e GOMODCACHE="$work/go-mod" -e GOFLAGS=-buildvcs=false \
  -e HANGAR_E2E_STRICT=1 \
  -e HANGAR_E2E_ALLOW_MISSING="${HANGAR_E2E_ALLOW_MISSING:-}" \
  -e HANGAR_E2E_URL="http://localhost:$port" \
  -e HANGAR_E2E_TOKEN="$token" \
  -e HANGAR_E2E_USER=admin -e HANGAR_E2E_PASSWORD="$admin_password" \
  -e HANGAR_TEST_S3_ENDPOINT="http://127.0.0.1:$minio_port" \
  -e HANGAR_TEST_S3_ACCESS_KEY_ID=hangar -e HANGAR_TEST_S3_SECRET_ACCESS_KEY=hangar-secret \
  "$runner_image" bash -euo pipefail -c '
    git config --global --add safe.directory "$PWD"
    echo "--- S3 blob store conformance against MinIO"
    go test -count=1 -v -run TestConformanceRealS3 ./internal/storage/s3/
    echo "--- real-client acceptance tests (HANGAR_E2E_STRICT=1)"
    go test -tags e2e -count=1 -timeout 60m -v -skip "TestImageScan|TestLDAP" "$@" ./test/e2e/...
  ' bash "$@"
