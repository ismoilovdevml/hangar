# Cargo: Rust crates

Hangar serves Cargo's [sparse registry protocol](https://doc.rust-lang.org/cargo/reference/registry-index.html#sparse-protocol)
(read by `cargo build`, `cargo update`, `cargo add`) and the
[registry web API](https://doc.rust-lang.org/cargo/reference/registry-web-api.html)
(`cargo publish`, `cargo yank`, `cargo search`, `cargo owner --list`).
It needs cargo 1.74 or newer (sparse registries with authentication).

Placeholders:

- `hangar.example.com`: the host of `server.base_url`.
- `<token>`: a personal access token (`hgr_...`).
- `$HANGAR_TOKEN`: the token in an environment variable.

URLs of a repository `<repo>`:

| Purpose | URL |
|---|---|
| Index (`[registries.*] index`) | `sparse+https://hangar.example.com/repository/<repo>/index/` |
| Web API (announced in `config.json`) | `https://hangar.example.com/repository/<repo>` |
| Downloads (announced in `config.json`) | `https://hangar.example.com/repository/<repo>/api/v1/crates/<crate>/<version>/download` |

Cargo sends the token as-is in the `Authorization` header. Hangar accepts the
raw `hgr_...` token, `Bearer <token>` and HTTP Basic.

## Repositories

### Hosted

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "cargo-hosted",
        "format": "cargo",
        "type": "hosted",
        "blob_store": "default",
        "config": { "hosted": { "write_policy": "allow_once" } }
      }'
```

- Versions are immutable: publishing a version that exists (also with other
  `+build` metadata) answers `409`, whatever the write policy. Yank it and
  publish a new version instead. `write_policy: deny` makes the repository
  read-only.
- Crate names follow the crates.io rules: ASCII letters, digits, `-` and `_`,
  starting with a letter, at most 64 characters; names such as `std`, `core`
  and Windows device names are refused. The first published spelling of a name
  is kept: `Serde` cannot be published once `serde` exists. As on crates.io,
  `-` and `_` are the same character in a name: once `foo-bar` exists,
  publishing `foo_bar` answers `409` naming the existing crate.
- Versions must be SemVer 2.0.0. The `.crate` must be a gzip tarball of
  `<name>-<version>/` with a `Cargo.toml` naming the same crate and version.
- Index files are generated from the stored versions on every request, with
  `ETag` and `Last-Modified`, so cargo's cache revalidates cheaply. Deleting a
  version (UI, REST API, cleanup policy) removes it from the index at once.

### Proxy (crates.io)

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "crates-io",
        "format": "cargo",
        "type": "proxy",
        "blob_store": "default",
        "config": { "proxy": { "remote_url": "https://index.crates.io", "metadata_ttl": "30m" } }
      }'
```

- `remote_url` is the remote's sparse index root (without `sparse+`).
- Index files are cached for `metadata_ttl` (default `30m`), then revalidated
  with the remote's ETag; while the remote is down the cached copy is served.
- `.crate` files are downloaded on first use from the `dl` template of the
  remote's `config.json` (crates.io: `https://static.crates.io/crates`),
  verified against the index `cksum` (sha256) and cached forever.
- `config.format.download_url` overrides the download location, in the
  syntax of `dl` (`{crate}`, `{version}`, `{prefix}`, `{lowerprefix}`,
  `{sha256-checksum}`; without markers `/{crate}/{version}/download` is
  appended). Its host is trusted for downloads; the remote's credentials are
  never sent there:

  ```json
  "format": { "download_url": "https://static.crates.io/crates/{crate}/{crate}-{version}.crate" }
  ```
- Hangar's own `config.json` points `dl` at Hangar, so clients never talk to
  the remote.
- A private sparse registry as the remote needs credentials that cargo would
  send raw: use `{"kind": "header", "header_name": "Authorization", "token": "<token>"}`
  as `config.proxy.auth`.
- `cargo search` against a proxy searches what it has cached.

### Group

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "cargo-group",
        "format": "cargo",
        "type": "group",
        "config": { "group": { "members": ["cargo-hosted", "crates-io"], "write_member": "cargo-hosted" } }
      }'
```

Index files merge the members in order: the first member that has a version
wins. `cargo publish` and `cargo yank` against the group go to `write_member`
(and need write access to both); without one they get `405`.

Name internal crates so they cannot collide with crates.io crates: the merged
index lists versions from every member, and cargo picks the highest match.

## Client setup

`~/.cargo/config.toml` (or `.cargo/config.toml` in the project):

```toml
[registry]
# Required for registries that need a token.
global-credential-providers = ["cargo:token"]

[registries.cargo-hosted]
index = "sparse+https://hangar.example.com/repository/cargo-hosted/index/"

[registries.cargo-group]
index = "sparse+https://hangar.example.com/repository/cargo-group/index/"

# Resolve every crates.io dependency through Hangar.
[source.crates-io]
replace-with = "cargo-group"
```

Store the token per registry (it goes to `$CARGO_HOME/credentials.toml`):

```bash
echo "$HANGAR_TOKEN" | cargo login --registry cargo-group
echo "$HANGAR_TOKEN" | cargo login --registry cargo-hosted
```

or, in CI, set it in the environment (registry name upper-cased, `-` as `_`):

```bash
export CARGO_REGISTRIES_CARGO_GROUP_TOKEN="$HANGAR_TOKEN"
export CARGO_REGISTRIES_CARGO_HOSTED_TOKEN="$HANGAR_TOKEN"
```

Without `global-credential-providers`, cargo refuses private registries with
"authenticated registries require a credential-provider to be configured". The
`cargo:token` provider keeps the token in plain text; `cargo:macos-keychain`,
`cargo:wincred` or `cargo:libsecret` keep it in the OS keychain instead.

A repository with anonymous read announces `"auth-required": false`, and cargo
then reads it without a token; publishing still needs one.

## Publish

`Cargo.toml` of the crate:

```toml
[package]
name = "acme-tiny"
version = "0.1.0"
edition = "2021"
description = "..."
license = "MIT"
publish = ["cargo-hosted"]   # never to crates.io by mistake
```

```bash
cargo publish --registry cargo-hosted
```

Dependencies on other crates of the same registry need `registry = "cargo-hosted"`
in `[dependencies]`; with `[source.crates-io] replace-with` set, plain
crates.io dependencies resolve through the group.

## Use

```bash
cargo add acme-tiny --registry cargo-hosted
cargo build
```

`cargo add` writes `acme-tiny = { version = "0.1.0", registry = "cargo-hosted" }`.
When the crate is reached through `[source.crates-io] replace-with`, a plain
`acme-tiny = "0.1"` works too.

## Yank, search, owners

```bash
cargo yank --registry cargo-hosted acme-tiny@0.1.0
cargo yank --undo --registry cargo-hosted acme-tiny@0.1.0
cargo search --registry cargo-hosted tiny
cargo owner --list --registry cargo-hosted acme-tiny
```

Yanked versions stay downloadable for existing lockfiles; new resolutions skip
them. Owners are Hangar permissions: anyone with `repo:<repo>:write` may
publish and yank. `cargo owner --list` shows who published the crate's
versions; `cargo owner --add/--remove` answers `501` with that explanation.
To delete a version, delete its component through the [REST API](api.md) or
the web UI.

## Notes

- Package pages in the web UI take the README, licence, keywords and
  repository from the `Cargo.toml` and README inside the `.crate`.
- Vulnerability scanning checks crates against the OSV `crates.io` ecosystem.
- Plain HTTP: cargo accepts `sparse+http://` index URLs, but the token is then
  sent in clear text; use HTTPS outside a test machine.

## Routing rules

A group that combines a hosted repository with a public proxy can serve a
public package published under one of your internal names. Block internal
names on the proxy and reserve them for the hosted member (for example `edcom-*`);
see [routing-rules.md](routing-rules.md).
