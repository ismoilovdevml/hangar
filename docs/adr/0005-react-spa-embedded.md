# 0005. React SPA embedded in the binary

Date: 2026-11-03 · Status: Accepted

## Context
The UI is a rich client (live feed, command palette, filters in the URL). It
must ship with the server version it was built against, without a separate web
server.

## Decision
- React + TypeScript + Vite in `web/`, TanStack Router and Query, CSS variables
  (no UI kit). It talks only to `/api/v1` as described by `api/openapi.yaml`
  and develops against MSW mocks generated from that contract.
- Release builds embed `web/dist`: the Dockerfile (and `make build-embed`)
  copies it to `internal/webui/dist` and compiles with `-tags webui`
  (`//go:embed all:dist`). Without the tag the binary serves a small fallback
  page, so Go builds and tests never need Node.
- Unknown paths that are not API, protocol or ops routes serve `index.html`
  (client-side routing); hashed files under `assets/` are cached immutably.

## Consequences
- One artifact; UI and API versions cannot drift in production.
- The web build is optional in CI's Go jobs and gated on `web/package.json`.
