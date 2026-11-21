// Package cac holds end-to-end tests of config-as-code (`hangar apply`,
// `hangar diff`, `hangar export`): they build the hangar binary, run
// `hangar serve` on a throwaway PostgreSQL schema and drive the CLI against
// it as a subprocess, exit codes included. They need HANGAR_TEST_DATABASE_URL
// and are skipped without it:
//
//	HANGAR_TEST_DATABASE_URL=postgres://... go test ./test/cac/
package cac
