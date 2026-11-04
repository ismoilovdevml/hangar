// Package security holds black-box security tests that run against a real
// `hangar serve` process: the authorization sweep over every REST API
// operation in api/openapi.yaml and every package format endpoint, SSRF
// checks for proxy repositories, encryption of stored secrets and the HTTP
// hardening headers.
//
// The tests build the hangar binary, start it on a free local port with a
// throwaway PostgreSQL schema and need HANGAR_TEST_DATABASE_URL; without it
// they are skipped:
//
//	HANGAR_TEST_DATABASE_URL=postgres://... go test ./test/security/
//
// They run in CI with the other database tests, so any route that answers
// an unauthorized caller with success fails the build.
package security
