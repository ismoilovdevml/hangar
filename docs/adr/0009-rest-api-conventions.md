# 0009. REST API conventions

Date: 2026-11-03 · Status: Accepted

## Context
The web UI and automation share one REST API, defined first in
`api/openapi.yaml` so the UI can generate types and mocks before endpoints
exist.

## Decision
- OpenAPI 3.1 in `api/openapi.yaml` is the contract; every API change updates
  it in the same change. A test fails if a route implemented in `internal/api`
  is missing from the file or if any `$ref` dangles.
- Base path `/api/v1`. JSON field names are `snake_case` (also in core types'
  JSON/YAML tags and in config). Timestamps RFC 3339 UTC, durations as Go
  duration strings, sizes in bytes.
- Lists use cursor pagination: `limit` + opaque `cursor`; responses carry
  `items` and `next_cursor` (null/absent on the last page).
- Errors: every non-2xx JSON response is `{"code", "message", "retry_after"?, "details"?}`.
- Actions are POST sub-resources (`/tasks/{id}/run`, `/tasks/{id}/stop`,
  `/cleanup-policies/preview`) rather than `:verb` suffixes, which do not route
  cleanly with path parameters.
- Live updates are Server-Sent Events at `GET /api/v1/events` (`event:` = event
  type, `data:` = JSON payload).
- Endpoints in the contract that are not built yet answer `501` with code
  `not_implemented`.

## Consequences
- Client generation (`openapi-typescript`) and mocks stay in sync with the server.
- Breaking changes require `/api/v2`.
