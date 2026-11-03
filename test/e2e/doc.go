// Package e2e holds the real-client acceptance tests from docs/FORMATS.md.
//
// Tests carry the build tag e2e and run against an already running Hangar:
//
//	HANGAR_E2E_URL=http://localhost:8080 go test -tags e2e ./test/e2e/...
//
// (`make e2e` does this.) Client tools run in containers, so Docker is
// required. One file per format: docker_test.go, npm_test.go, ...
package e2e
