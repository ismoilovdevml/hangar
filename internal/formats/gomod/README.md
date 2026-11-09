# `go` format

Implements **Go modules**: the [GOPROXY protocol](https://go.dev/ref/mod#goproxy-protocol)
read by the `go` command, an upload API for private modules, and a
pass-through of the [checksum database](https://go.dev/ref/mod#checksum-database).
Protocol requirements and the acceptance checklist are in
[docs/FORMATS.md](../../../docs/FORMATS.md), section "Go"; client setup is in
[docs/guides/go.md](../../../docs/guides/go.md).

The package is `gomod` because `go` is a keyword; the format name is `go`.

## Client configuration

```
GOPROXY=https://hangar.example.com/repository/go-group
GOPRIVATE=git.example.com/acme GONOPROXY=none GONOSUMDB=git.example.com/acme
# ~/.netrc (sent by the go command over HTTPS only)
machine hangar.example.com login token password <token>
```

## Routes (relative to `/repository/{repo}/`)

`{module}` and `{version}` are case-encoded (`!a` for `A`).

| Method | Path | |
|---|---|---|
| GET, HEAD | `{module}/@v/list` | known versions without pseudo-versions; `404` for an unknown module |
| GET, HEAD | `{module}/@latest` | `.info` of the latest version (release > pre-release > pseudo-version) |
| GET, HEAD | `{module}/@v/{version}.info` / `.mod` / `.zip` | per-version files; proxies also resolve non-version queries (`master.info`) |
| PUT | `{module}/@v/{version}.zip` / `.mod` / `.info` | publish (hosted, or a group's `write_member`); `repo:<repo>:write` |
| DELETE | `{module}/@v/{version}` | delete a version; `repo:<repo>:delete` |
| GET, HEAD | `sumdb/{name}/supported`, `latest`, `lookup/...`, `tile/...` | checksum database pass-through (proxy, group) |

Missing modules and versions answer `404` (also for a remote `410`), so
`GOPROXY=a,b` falls back; remote failures answer `502`.

## Storage

- One component per module version: namespace empty, name = module path as
  written (not case-encoded), version = canonical version. Attributes:
  `complete` (the zip is stored: listed and served), `time` (the `.info`
  Time), `has_go_mod`, `go_version`.
- Assets at `{module}/@v/{version}/{version}.info|.mod|.zip` (kind `content`).
  The handler also serves these storage paths, which is what asset download
  links point at.
- **Hosted**: the zip is streamed to the blob store, then validated with
  random access against the module zip rules (prefix `{module}@{version}/`,
  clean and valid file paths, no case-insensitive collisions, `go.mod` only
  at the root, 500 MiB zip and content, 16 MiB `go.mod` and `LICENSE`).
  The version must be canonical semver and pass `module.Check` (major version
  suffix, `gopkg.in`, `+incompatible` only without `go.mod`); `go.mod` must
  declare the module. The `.mod` is the zip's `go.mod` (or `module {path}`);
  the `.info` Time is the upload time unless a `.info` was uploaded first. A
  `.mod` uploaded first must equal the zip's `go.mod`. `allow_once`: the same
  zip again answers `200`, other content `409`.
- **Proxy**: `@v/list` and `@latest` are cached at `{module}/@v/list` and
  `{module}/@latest` (kind `metadata`) for `proxy.metadata_ttl` (default
  `30m`), revalidated with ETag / Last-Modified, served stale when the remote
  fails. Per-version files are fetched once (collapsed per path), checked
  (zips against the rules above, `.info` against the version) and cached
  forever. Non-version queries are passed through uncached. All fetches use
  the repository's upstream (SSRF guard, breaker, outbound proxy, negative
  cache, Retry-After, offline mode). Cache fills are published as the reading
  principal.
- **Group**: `@v/list` is the union of the members; per-version files come
  from the first member that has them; `@latest` is the best answer across
  members. Writes go to `group.write_member`, else `405`.
- **Checksum database**: proxies forward `sumdb/{name}/...` to
  `https://{name}/...` as a related host (`sumdb_via: database`, default) or to
  `{remote}/sumdb/{name}/...` (`sumdb_via: remote`); `supported` answers `200`
  locally in database mode. Nothing is cached. Hosted repositories answer
  `404` (the go command then contacts the database itself); groups delegate
  to their first proxy member.
- Quarantined versions are left out of `list` and `@latest`; their files
  answer `403`. Zip downloads are recorded (download count, last download).

## Format config

```yaml
config:
  format:
    sumdbs: [sum.golang.org]   # proxy only; [] turns the pass-through off
    sumdb_via: database        # or: remote
```

## Tests

- Unit tests (`go test ./internal/formats/gomod/`) use the `coretest` fakes:
  hosted publish/validation/write-once/staging/authorization/quarantine,
  proxy caching, TTL, revalidation, stale, 404/410/5xx semantics, case
  encoding, queries, offline, sumdb modes, group merging.
- `TestRealGoClient` drives the real `go` command (TLS test server, isolated
  `GOMODCACHE`/`GOPATH`/`HOME`, `GOFLAGS=-modcacherw`): `go mod tidy`,
  `go mod download` and `go build` through a proxy of proxy.golang.org with
  the checksum database passed through; a private module zip built with
  `golang.org/x/mod/zip` published to hosted and consumed through a group with
  `GOPRIVATE`, `GONOPROXY=none`, `GONOSUMDB` and `~/.netrc`. Needs network
  access to proxy.golang.org and sum.golang.org; skipped otherwise and with
  `-short`.
- Acceptance against a running server: `test/e2e/go_test.go` (build tag `e2e`).
