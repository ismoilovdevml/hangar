# 0001. Go and chi, one `hangar` binary

Date: 2026-11-03 · Status: Accepted

## Context
Hangar serves many wire protocols (OCI, npm, Maven, PyPI, ...) with large
streaming uploads and downloads, a REST API and a web UI. Operators want a
single artifact that is easy to run as a container, a systemd service or a
Kubernetes deployment, with low memory per connection.

## Decision
- Go (version pinned in `go.mod`), standard `net/http` plus
  [chi](https://github.com/go-chi/chi) for routing. chi is a thin layer over
  `net/http` (handlers stay `http.Handler`), supports sub-routers and route
  patterns we use as metric labels.
- One binary, `cmd/hangar`, with subcommands `serve`, `migrate`, `version`
  (admin subcommands later). Configuration from `hangar.yaml` plus `HANGAR_*`
  environment variables (environment wins), validated at start.
- Structured JSON logs with `log/slog` (request id on every line), Prometheus
  metrics on `/metrics`, `/healthz` and `/readyz`.
- No global HTTP read/write timeouts on the server (multi-GB transfers);
  `ReadHeaderTimeout` and idle timeouts bound slow clients, and handlers use
  the request context.

## Consequences
- Static, CGO-free binaries for every platform; distroless runtime image.
- No framework magic: middleware and handlers are plain functions, easy to test
  with `httptest`.
- Adding a protocol is adding a package (see 0007), not a process.
