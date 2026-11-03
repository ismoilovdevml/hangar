// Package core defines Hangar's domain model and the contracts between its parts.
//
// Everything outside core (format handlers, storage drivers, the REST API, the
// job runner) depends on the types and interfaces declared here, never on each
// other's implementations. The rules every implementation follows:
//
//   - Blobs are content-addressed by sha256 and deduplicated per blob store.
//     Bytes are streamed, never buffered whole in memory.
//   - Every mutating service call authorizes the Principal carried in the
//     context (see WithPrincipal) and records an AuditEvent.
//   - Format handlers authorize at the HTTP boundary too, because only they
//     know how to phrase the client-specific challenge (WWW-Authenticate for
//     OCI, 401 JSON for npm, ...). Services repeat the check as defence in depth.
//   - Errors are reported with the sentinel values in errors.go, wrapped with
//     fmt.Errorf("...: %w", err). Callers test them with errors.Is.
//
// In-memory implementations of every interface live in package coretest, so
// format packages can be unit-tested without PostgreSQL, S3 or NATS.
package core
