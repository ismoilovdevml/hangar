# `cargo` format

Implements the **Cargo** format: the sparse index protocol read by cargo and
the registry web API used by `cargo publish`, `yank`, `search` and `owner`.
Protocol requirements and the real-client acceptance checklist are in
[docs/FORMATS.md](../../../docs/FORMATS.md), section "Cargo"; client setup is
in [docs/guides/cargo.md](../../../docs/guides/cargo.md).

Specs: [registry index](https://doc.rust-lang.org/cargo/reference/registry-index.html),
[web API](https://doc.rust-lang.org/cargo/reference/registry-web-api.html),
[registry authentication](https://doc.rust-lang.org/cargo/reference/registry-authentication.html).

## Routes (relative to `/repository/{repo}/`)

| Method | Path | |
|---|---|---|
| GET, HEAD | `index/config.json` | `dl`, `api`, `auth-required`; `401` + `WWW-Authenticate: Cargo` for anonymous clients of private repositories |
| GET, HEAD | `index/{1,2,3/c,ab/cd}/{name}` | index file, one JSON line per version; `ETag`, `Last-Modified`, conditional `304`; non-canonical paths `404` |
| PUT | `api/v1/crates/new` | publish: u32 LE metadata length, JSON, u32 LE crate length, `.crate` |
| DELETE | `api/v1/crates/{name}/{version}/yank` | yank |
| PUT | `api/v1/crates/{name}/{version}/unyank` | un-yank |
| GET, HEAD | `api/v1/crates/{name}/{version}/download` | the `.crate` (the `dl` target; name case-insensitive) |
| GET | `api/v1/crates?q=&per_page=` | search (name and description, every word) |
| GET | `api/v1/crates/{name}/owners` | principals that published the crate |
| PUT, DELETE | `api/v1/crates/{name}/owners` | `501`: ownership is `repo:<name>:write` |

Errors are `{"errors":[{"detail":"..."}]}`, which cargo prints.
`Authorization` may hold the raw token (what cargo sends), `Bearer <token>` or
Basic credentials.

## Storage

- **Hosted**: one component per version (namespace empty, name lowercased,
  attributes `display_name`, `index_entry` = the version's index line,
  `yanked`, `index_updated_at`, `description`, `license`, `homepage`,
  `repository`, `documentation`, `keywords`, `categories`, `authors`,
  `rust_version`, `prerelease`) and one content asset at
  `crates/{name}/{version}/{name}-{version}.crate`. The `.crate` streams to the
  blob store; its sha256 is the index `cksum`. The tarball must hold
  `{name}-{version}/Cargo.toml` naming the same crate and version, and nothing
  outside that directory (no links). Versions are write-once regardless of
  write policy (build metadata ignored), and the first spelling of a name is
  kept. Publish metadata becomes the index line: `explicit_name_in_toml`
  renames become `name` + `package`, features using `dep:` or `?/` move to
  `features2` with `v: 2`. Index files are rendered from the components on
  each request, so deletes and cleanup policies apply at once; quarantined
  versions are left out and their downloads answer `403`.
- **Proxy**: index files are fetched from `remote_url` + index path, cached at
  `index/{path}` (metadata) for `proxy.metadata_ttl` (default 30m), then
  revalidated with ETag / Last-Modified; a stale copy is served while the
  remote fails. Crates are downloaded from `config.format.download_url`, else
  the `dl` of the remote's `config.json` (cached like an index file), else
  `https://static.crates.io/crates/{crate}/{crate}-{version}.crate`; they are
  verified against the index `cksum`, cached under the hosted `crates/`
  layout and never re-fetched. The download host is passed to the core
  upstream as a related host (SSRF guard, breaker, collapsing, outbound proxy;
  no remote credentials). Cache fills are published as the reading principal.
- **Group**: index files merge members in order, first member wins per
  version (build metadata ignored); downloads resolve members in order;
  publish and yank go to `group.write_member` (hosted cargo), else `405`.

Every repository serves its own `config.json`: `dl` is
`{base}/repository/{repo}/api/v1/crates` and `api` `{base}/repository/{repo}`,
both from `server.base_url`.

## Format config

```yaml
config:
  format:
    download_url: https://static.crates.io/crates/{crate}/{crate}-{version}.crate   # proxy only
```

## Tests

- Unit tests (`go test ./internal/formats/cargo/`) use the `coretest` fakes.
- `TestRealCargo` drives the installed `cargo` (PATH or `~/.cargo/bin`) with an
  isolated `CARGO_HOME`: `cargo package` of a stand-in crate served by a local
  sparse index, `cargo publish` into a private hosted repository, `cargo build`
  / `cargo run` of a crate depending on it and on the stand-in through a group
  (`[source.crates-io] replace-with`), `cargo search`, `cargo yank` / `--undo`,
  `cargo owner`, and resolution without a token and with a wrong one. Skipped
  with `-short` or without cargo.
- `TestRealCargoCratesIO` builds against a proxy of the real crates.io; it
  runs only with `HANGAR_TEST_CRATES_IO=1`.
- Acceptance against a running server: `test/e2e/cargo_test.go` (build tag `e2e`).
