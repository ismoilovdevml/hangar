# `npm` format

Implements the **npm** registry protocol for hosted, proxy and group
repositories. Protocol requirements and the real-client acceptance checklist
are in [docs/FORMATS.md](../../../docs/FORMATS.md), section "npm".

Registry URL for clients: `https://<hangar>/repository/<repo>/`.

## Endpoints (relative to `/repository/<repo>/`)

| Method | Path | Notes |
|---|---|---|
| GET/HEAD | `<pkg>` | packument; abbreviated (`application/vnd.npm.install-v1+json`) when the client asks for it, full otherwise or with `?write=true`. ETag / `If-None-Match`. |
| GET | `<pkg>/<version\|tag>` | one version document |
| GET/HEAD | `<pkg>/-/<file>.tgz` | tarball; Range supported; counts a download |
| PUT | `<pkg>` | `npm publish` (base64 attachment streamed into the blob store, shasum + integrity verified, versions immutable); without an attachment: packument update (`npm deprecate`, undeprecate, version removal) |
| PUT | `<pkg>/-rev/<rev>` | packument update sent by `npm unpublish pkg@version` |
| DELETE | `<pkg>/-rev/<rev>` | unpublish every version |
| DELETE | `<pkg>/-/<file>.tgz/-rev/<rev>` | last step of `npm unpublish pkg@version` (idempotent) |
| GET | `-/package/<pkg>/dist-tags[/<tag>]` | dist-tags |
| PUT/POST/DELETE | `-/package/<pkg>/dist-tags/<tag>` | `npm dist-tag add/rm`; `latest` cannot be removed; tags may not be versions |
| PUT | `-/user/org.couchdb.user:<name>` | `npm login` (username/password → session token via the token issuer) |
| DELETE | `-/user/token/<token>` | `npm logout`: revokes the token when `npm login` minted it (claim `npm_login`, via `core.TokenRevoker`); other tokens are left alone; answered 200 |
| GET | `-/v1/search` | `npm search`: hosted from the component search, proxy from the remote (routing rules applied), group merged in member order |
| GET | `-/whoami`, `-/ping` | |
| POST | `-/npm/v1/security/advisories/bulk`, `-/npm/v1/security/audits[/quick]` | `npm audit` (gzip bodies): proxy names (routing rules applied) go to the remote's bulk endpoint, hosted versions are answered from stored findings (`Format.SetAdvisories`); 503 when a hosted version has no scan result, 502 when a remote fails (see audit.go) |

`<pkg>` is `name` or `@scope/name`; `@scope%2fname` (and `%40scope%2fname`)
are accepted. `POST -/v1/login` answers 404 so npm falls back to the
username/password login.

## Repository types

- **hosted** — clients publish here. Write policy `deny` refuses publishes;
  versions are always immutable (403 like the public registry).
- **proxy** — packuments (full and abbreviated cached separately) are served
  from cache for `metadata_ttl` (shared proxy freshness policy), then
  revalidated with a conditional request; a stale copy is served when the
  remote fails. Tarball URLs are rewritten to Hangar (`server.base_url`).
  Tarballs are fetched once, verified against the packument's
  shasum/integrity, and cached as components. Cache fills are published as
  the requesting principal (a proxy publish is authorized as a read); a read
  through a group fills a proxy member's cache on the group's
  `repo:<group>:read`.
- **group** — packuments of the members are merged in member order: the first
  member that has a version or dist-tag wins. Tarballs resolve in member order.
  Writes (publish, dist-tag, deprecate, unpublish) go to `group.write_member`;
  without one the group is read-only (405).

## Configuration (`config.format`)

| Key | Default | Meaning |
|---|---|---|
| `unpublish_window` | `72h` | how long after publishing a principal with `repo:<name>:delete` may unpublish; `0s` = admins only. `repo:<name>:admin` may always unpublish. Unpublishing rewrites the package state (dist-tags, tombstones), which needs `repo:<name>:write`. |
| `reuse_unpublished_versions` | `false` | By default an unpublished version leaves a tombstone in the package state and the version number cannot be published again (403, as on the public registry). `true` allows reuse. |
| `login_token_ttl` | `720h` | lifetime of tokens issued by `npm login` |

## Storage layout

```
<pkg>/-/<base>-<version>.tgz          tarball            content,  version component
<pkg>/-/meta/<version>.json           version manifest   metadata, version component (hosted)
<pkg>/-/package.json                  dist-tags, times   metadata (hosted)
<pkg>/-/packument.json                upstream document  metadata (proxy)
<pkg>/-/packument.abbreviated.json    upstream document  metadata (proxy)
```

Components: namespace = scope (`@acme`), name = unscoped name, version.
Attributes: `description`, `license`, `deprecated`. Only `.tgz` content
assets are served on the tarball route.

## Known limits

- Packuments of hosted repositories are rendered per request from the
  version manifests (one blob read per version).
- Package-state updates are serialised per node; concurrent publishes of the
  same package on different nodes can race on dist-tags.
- Unpublished version numbers can be published again (no tombstones).
- Web login, `npm access`/`npm owner`/teams and
  provenance attachments (`.sigstore`, accepted but not stored) are not implemented.

## Tests

- `go test ./internal/formats/npm/` — unit tests with `coretest` fakes and the
  real `npm`, `pnpm` and `yarn` (classic) binaries against an in-process
  server (skipped when a binary is missing or with `-short`).
- `test/e2e/npm_test.go` (build tag `e2e`) — the same flows against a running
  Hangar with a registry.npmjs.org proxy, including yarn berry when installed.

## Concurrency and caching

- Package state changes (publish, dist-tags, deprecate, unpublish) take a
  per-package lock from `Deps.Locks`: a PostgreSQL advisory lock in production,
  so two nodes of a cluster cannot lose each other's dist-tag updates.
- Version manifests and package state documents are cached in memory by blob
  digest (64 MiB LRU). Blobs are immutable, so the cache needs no invalidation;
  a packument read costs two queries (versions and manifest listing) plus the
  state lookup, and no blob reads once warm.
