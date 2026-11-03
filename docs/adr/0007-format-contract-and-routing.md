# 0007. Format contract and request routing

Date: 2026-11-03 · Status: Accepted

## Context
Ten formats will be implemented independently and in parallel. They must share
storage, authentication, authorization, audit and events without depending on
each other or on infrastructure packages.

## Decision
- A format is a package `internal/formats/<name>` implementing `core.Format`
  (`Name`, `Mount(chi.Router, core.Deps)`, `Validate`, `Coordinates`) and
  depending only on `internal/core`. Everything it uses arrives in `core.Deps`
  (repositories, components, blob stores, upstreams, HTTP clients,
  authentication, token issuing, authorization, events, jobs, audit, logger,
  base URL). Formats never import storage drivers, the database or each other.
- Built-in formats are listed in `internal/formats/builtin`; the registry
  rejects duplicate names.
- Routing:
  - `/repository/{repo}/...` for every format. A shared dispatcher resolves the
    repository, rejects unknown (404), foreign-format (404) and offline (503)
    repositories, stores it in the context (`core.RepositoryFrom`) and hands
    the remainder of the path to the format's own router. Route patterns
    become `/repository/{repo}/<format route>` for metrics.
  - Formats whose clients need the host root implement `core.RootMounter`
    (OCI: `/v2/`). Such formats may also serve a repository through a
    connector (`RepoConfig.Connector`: dedicated host name or port).
- Authorization happens twice: format handlers check at the HTTP boundary so
  they can answer with their protocol's challenge, and services re-check every
  mutation (`repo:<name>:write|delete`) and write the audit record.
- Writes go through `ComponentService.Publish` in one transaction (write
  policy, ref counts, audit, `component.published`).
- `internal/core/coretest` provides in-memory fakes of every interface and
  `formats.NewHandler(deps, f)` mounts a format exactly as production does, so
  format tests need no database.

## Consequences
- Format packages can be built and tested in isolation; interface changes go
  through `internal/core` and are reviewed as contract changes.
- Cross-cutting behaviour (quarantine, download stats, write policy) lives in
  one place.
