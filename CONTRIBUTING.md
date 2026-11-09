# Contributing

Requirements: Go (see `go.mod`), PostgreSQL 16 or newer, Node 22 (`web/.nvmrc`)
for the web UI, and for the acceptance tests Docker on Linux.

```bash
make dev     # postgres, minio, nats and trivy in docker, then the server and the web dev server
make help    # every target
```

Changes come in small pull requests with tests. Before you open one, run the
checks below; the release workflow runs the same ones on every tagged
commit and publishes nothing unless all of them pass.

## Unit and integration tests

```bash
createdb hangar_test
export HANGAR_TEST_DATABASE_URL='postgres://localhost:5432/hangar_test?sslmode=disable'
make test    # go test -race (cmd, internal, test) and vitest
make lint    # golangci-lint, eslint, tsc
```

About a quarter of the Go tests need PostgreSQL, so `make test` stops with an
error when `HANGAR_TEST_DATABASE_URL` is not set. To run without them anyway,
`HANGAR_TEST_SKIP_DB=1 make test` runs everything else and ends with the
number of skipped tests; such a run does not count as a passing build.

The S3 blob store has a conformance test against a real endpoint, skipped
unless `HANGAR_TEST_S3_ENDPOINT` is set:

```bash
make dev-deps
HANGAR_TEST_S3_ENDPOINT=http://127.0.0.1:9000 HANGAR_TEST_S3_ACCESS_KEY_ID=hangar \
HANGAR_TEST_S3_SECRET_ACCESS_KEY=hangar-secret \
  go test -count=1 -v -run TestConformanceRealS3 ./internal/storage/s3/
```

Contract checks that run with the unit tests:

- `internal/api` routes and `api/openapi.yaml` agree in both directions:
  every route is documented, and every documented operation is routed or
  marked `x-status: planned` (it then answers 501 `not_implemented`).
- `web/src/test/mswContract.test.ts`: the MSW mock server only mocks
  operations of `api/openapi.yaml`.
- `test/security`: every operation of the spec has an authorization policy
  and refuses callers who must be refused.

After changing `api/openapi.yaml`, regenerate the web types with
`npm --prefix web run gen:api`.

## Real-client acceptance tests (`test/e2e`)

The e2e suite drives the real clients (docker, npm, pnpm, yarn, mvn, gradle,
pip, twine, uv, poetry, go, helm, dotnet, cargo, curl, wget, crane, cosign)
against a running server. Its build tag is `e2e`.

All in containers, on a Linux host with Docker (what CI runs):

```bash
make e2e-docker          # or: test/e2e/run-docker.sh -run 'TestHelm|TestRPM'
```

It builds the Hangar image and the client image (`test/e2e/runner/Dockerfile`),
starts PostgreSQL, MinIO and Hangar on the host network, runs the S3
conformance test against MinIO and then the whole suite with
`HANGAR_E2E_STRICT=1`. The script's header lists its settings.

Against a server you started yourself, with the clients installed locally:

```bash
HANGAR_E2E_URL=http://localhost:8080 HANGAR_E2E_TOKEN=hgr_... \
HANGAR_E2E_USER=admin HANGAR_E2E_PASSWORD=... make e2e
```

- The token needs `repo:*:admin,system:*:admin`
  (`hangar admin token create --user admin --name e2e --scopes 'repo:*:admin,system:*:admin'`).
- Start the server with `HANGAR_SECRET_KEY` (APT and RPM signing keys) and
  with `HANGAR_HTTP_ALLOW_PRIVATE_NETWORKS=127.0.0.0/8,::1/128,localhost`:
  the raw proxy test uses the server itself as its remote, and the outbound
  address policy refuses loopback otherwise (the test then fails with 502).
- A missing prerequisite skips its test with a `SKIP (set HANGAR_E2E_STRICT=1 ...)`
  line. With `HANGAR_E2E_STRICT=1` it fails instead;
  `HANGAR_E2E_ALLOW_MISSING=gradle,poetry` names clients that may be absent.
- The RPM proxy test picks the Rocky Linux mirror for the machine's
  architecture (x86_64 or aarch64); `HANGAR_E2E_RPM_REMOTE` overrides it.
- Helm 3 and Helm 4 both work.
- On macOS with Docker Desktop the daemon cannot reach the host's
  `localhost`, so the Docker, APT and RPM tests need a forwarder (for example
  an `alpine/socat` container on the host network forwarding to
  `host.docker.internal`) and `HANGAR_E2E_CONTAINER_URL`; Linux needs neither.
- `TestImageScan` brings its own Trivy stack:
  `HANGAR_E2E_IMAGE_SCAN_STACK=1 go test -tags e2e -run TestImageScan -timeout 60m -v ./test/e2e/`.
- LDAP and Active Directory: `test/e2e/ldap/run.sh`.

## Web UI

```bash
cd web
npm ci
npm run lint && npm run typecheck && npm test && npm run build
CI=1 npx playwright test           # against the mock (MSW) build; E2E_PORT picks the port
```

`web/e2e/live` is a Playwright smoke test against a real server (sign in,
create a repository and a token, a reader's read-only view and the server's
403). Build the server with the UI embedded (`make build-embed`), start it
with an administrator who can sign in, then:

```bash
E2E_LIVE_URL=http://localhost:8080 E2E_LIVE_USER=admin E2E_LIVE_PASSWORD=... \
E2E_LIVE_TOKEN=hgr_... make web-e2e-live
```

The live event stream keeps a connection open, so Playwright's
`networkidle` never settles; wait for elements instead.

## Deployment artifacts

```bash
make deploy-check   # helm lint and template, compose config, shellcheck, installer tests
```

## Changelog

Add a line under `## Unreleased` in `docs/changelog.md` for every change a
user or operator would notice.
