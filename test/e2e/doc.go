// Package e2e holds the real-client acceptance tests from docs/FORMATS.md.
//
// Tests carry the build tag e2e and run against an already running Hangar:
//
//	HANGAR_E2E_URL=http://localhost:8080 go test -tags e2e ./test/e2e/...
//
// (`make e2e` does this.) The tests call the real clients (docker, npm,
// pnpm, yarn, mvn, gradle, pip, twine, uv, poetry, go, helm, dotnet, cargo,
// curl, wget, crane, cosign) found on PATH of the machine running `go test`.
// Only the APT and RPM tests start containers of their own (Debian and
// Rocky Linux, through docker), so those two need a Docker daemon. CI runs
// the whole suite inside an image that carries every client
// (test/e2e/runner/Dockerfile, see CONTRIBUTING.md).
//
// A missing prerequisite (URL, credentials, client, Docker daemon) skips
// the test, and the skip says so. With HANGAR_E2E_STRICT=1 it fails
// instead; HANGAR_E2E_ALLOW_MISSING=gradle,poetry names clients that may
// still be absent. One file per format: docker_test.go, npm_test.go, ...
package e2e
