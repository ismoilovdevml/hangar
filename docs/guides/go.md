# Go modules: GOPROXY, private modules, checksum database

Hangar speaks the [GOPROXY protocol](https://go.dev/ref/mod#goproxy-protocol):
the `go` command downloads modules from it like from `proxy.golang.org`. A proxy
repository caches a public (or internal) module proxy, a hosted repository holds
your private modules, and a group serves both under one URL.

Placeholders:

- `hangar.example.com`: the host of `server.base_url`.
- `<token>`: a personal access token (`hgr_...`).
- `$HANGAR_TOKEN`: the token in an environment variable.

URLs of a repository `<repo>`:

| Purpose | URL |
|---|---|
| `GOPROXY` | `https://hangar.example.com/repository/<repo>` |
| Upload (hosted) | `PUT https://hangar.example.com/repository/<repo>/<module>/@v/<version>.zip` |
| Checksum database (proxy, group) | `https://hangar.example.com/repository/<repo>/sumdb/sum.golang.org/...` |

Module paths and versions in URLs are case-encoded as the protocol requires:
every upper-case letter becomes `!` plus the lower-case letter
(`github.com/Azure/x` → `github.com/!azure/x`).

## Repositories

### Proxy (proxy.golang.org)

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "go-proxy",
        "format": "go",
        "type": "proxy",
        "blob_store": "default",
        "config": { "anonymous_read": true, "proxy": { "remote_url": "https://proxy.golang.org", "metadata_ttl": "30m" } }
      }'
```

- `remote_url` can be any GOPROXY: `https://proxy.golang.org`, an internal
  Athens, another Hangar (`https://other/repository/go-group`).
- `@v/list` and `@latest` are cached for `metadata_ttl` (default `30m`), then
  revalidated; when the remote fails, the cached copy is served.
- `.info`, `.mod` and `.zip` of a version are immutable: fetched once, then
  served from the cache forever. Remote zips are checked against the module zip
  rules before they are cached.
- Queries that are not versions (`go get example.com/m@main`, `@<commit>`) are
  resolved by the remote on every request and not cached.
- Remote `404` and `410` answers become `404`, so `GOPROXY=hangar,direct`
  falls back as the go command expects; other remote failures answer `502`,
  which stops a `,` list (use `|` to fall back on any error).
- With `"offline": true` in `config.proxy`, only cached content is served.

### Hosted

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "go-hosted",
        "format": "go",
        "type": "hosted",
        "blob_store": "default",
        "config": { "hosted": { "write_policy": "allow_once" } }
      }'
```

Use `allow_once`: a published Go version must never change (every `go.sum` that
recorded it would break). Uploading the same zip again succeeds; different bytes
for a published version get `409`.

### Group

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "go-group",
        "format": "go",
        "type": "group",
        "config": { "group": { "members": ["go-hosted", "go-proxy"], "write_member": "go-hosted" } }
      }'
```

- `@v/list` merges the members' versions; for each version's files the first
  member that has it wins, so a hosted module is never fetched from the proxy.
- `@latest` is the best answer across members (releases before pre-releases
  before pseudo-versions).
- Uploads to the group go to `write_member`; without one they get `405`.
- Version lists and `@latest` ask every member, so the path of a private
  module listed through a group that also holds a proxy reaches that proxy's
  remote (as a request that answers `404`). Where module paths themselves are
  confidential, serve private modules from a group without the public proxy.

## Using it from the go command

Public modules only:

```bash
go env -w GOPROXY=https://hangar.example.com/repository/go-proxy,direct
```

Private modules (hosted) and public ones through one group:

```bash
go env -w GOPROXY=https://hangar.example.com/repository/go-group
go env -w GOPRIVATE=git.example.com/acme      # your private module prefix
go env -w GONOPROXY=none                       # ...but still fetch it through Hangar
go env -w GONOSUMDB=git.example.com/acme       # never ask sum.golang.org about it
```

`GOPRIVATE` alone would make the go command skip the proxy for those modules
and clone them from version control; `GONOPROXY=none` keeps them on Hangar.
`GONOSUMDB` (implied by `GOPRIVATE`) keeps their names away from the public
checksum database, which cannot know them.

Credentials go in `~/.netrc` (or the file named by `NETRC`). The go command
sends them only over HTTPS. Include the port in `machine` when the URL has one
(`machine hangar.example.com:8443`):

```
machine hangar.example.com
login token
password <token>
```

Any login works with a token as the password; your username and password work
too. Requests without credentials to a repository without anonymous read get
`401`.

CI (GitLab CI as an example):

```yaml
build:
  script:
    - printf 'machine hangar.example.com login token password %s\n' "$HANGAR_TOKEN" > ~/.netrc
    - export GOPROXY=https://hangar.example.com/repository/go-group GOPRIVATE=git.example.com/acme GONOPROXY=none
    - go build ./...
```

## Publishing a private module

A version is published by uploading its module zip:

```bash
curl -u token:"$HANGAR_TOKEN" --upload-file v1.2.3.zip \
  https://hangar.example.com/repository/go-hosted/git.example.com/acme/lib/@v/v1.2.3.zip
```

Hangar checks the zip against the [module zip rules](https://go.dev/ref/mod#zip-files):
every file under `<module>@<version>/`, valid file names without
case-insensitive collisions, `go.mod` only at the root, at most 500 MiB (and
16 MiB for `go.mod` and `LICENSE`). The version must be canonical semver
(`v1.2.3`, `v1.2.3-rc.1`) and agree with the module path's major version
(`/v2` for `v2.x.y`, `+incompatible` only without a `go.mod`). The zip's
`go.mod` must declare the module path. Its `go.mod` becomes the `.mod` file
(a module without one gets `module <path>`), and the `.info` Time is the upload
time.

Optionally upload the `.mod` and `.info` first: the `.mod` must then equal the
zip's `go.mod`, and the `.info` Time is kept:

```bash
base=https://hangar.example.com/repository/go-hosted/git.example.com/acme/lib/@v
curl -u token:"$HANGAR_TOKEN" --upload-file go.mod "$base/v1.2.3.mod"
curl -u token:"$HANGAR_TOKEN" -X PUT -d '{"Version":"v1.2.3","Time":"2026-01-02T03:04:05Z"}' "$base/v1.2.3.info"
curl -u token:"$HANGAR_TOKEN" --upload-file v1.2.3.zip "$base/v1.2.3.zip"
```

The version is listed and served only once its zip is stored. Answers: `201`
published, `200` the same content was already there, `400` invalid, `409`
the version exists with other content.

### Building the zip

The reliable way is `golang.org/x/mod/zip`, the code the go command itself
uses: it leaves out nested modules, most of `vendor/`, symlinks and files the
go command would refuse. A complete helper, `tools/modzip/main.go` (in a
module that requires `golang.org/x/mod`):

```go
// Command modzip writes the module zip of the module in dir.
// Usage: modzip <module path> <version> <dir> > <version>.zip
package main

import (
	"log"
	"os"

	"golang.org/x/mod/module"
	"golang.org/x/mod/zip"
)

func main() {
	if len(os.Args) != 4 {
		log.Fatal("usage: modzip <module path> <version> <dir>")
	}
	m := module.Version{Path: os.Args[1], Version: os.Args[2]}
	if err := zip.CreateFromDir(os.Stdout, m, os.Args[3]); err != nil {
		log.Fatal(err)
	}
}
```

A CI job that publishes the tag being built:

```bash
MODULE=$(go list -m)
VERSION=$CI_COMMIT_TAG                       # e.g. v1.2.3
go run ./tools/modzip "$MODULE" "$VERSION" . > "$VERSION.zip"
curl -fsS -u token:"$HANGAR_TOKEN" --upload-file "$VERSION.zip" \
  "https://hangar.example.com/repository/go-hosted/$MODULE/@v/$VERSION.zip"
```

Module paths with upper-case letters must be case-encoded in the URL
(`Acme` → `!acme`).

Without a helper, `git archive` produces an acceptable zip for a simple
module at the repository root (no nested modules, no `vendor/`, no
symlinks):

```bash
git archive --format=zip --prefix="$MODULE@$VERSION/" -o "$VERSION.zip" "$VERSION"
```

When such an archive breaks the rules, Hangar rejects the upload and names the
offending files.

### Deleting a version

```bash
curl -u token:"$HANGAR_TOKEN" -X DELETE \
  https://hangar.example.com/repository/go-hosted/git.example.com/acme/lib/@v/v1.2.3
```

Needs `repo:<repo>:delete`; audited. Builds whose `go.sum` lists the version can
no longer download it, so prefer publishing a new version (and a
[`retract`](https://go.dev/ref/mod#go-mod-file-retract) directive) over deleting.

## Checksum database

For public modules the go command verifies `go.sum` lines against
`sum.golang.org`. When `GOPROXY` points at a proxy repository (or a group with
one), the go command asks `<GOPROXY>/sumdb/sum.golang.org/supported` and then
sends its `lookup`, `latest` and `tile` requests to Hangar, which forwards
them. Builds therefore work where only Hangar can reach the internet; the go
command still checks the database's signatures itself, so Hangar cannot alter
the answers.

| `config.format` key | Meaning |
|---|---|
| `sumdbs` | database names served under `sumdb/` (default `["sum.golang.org"]`; `[]` turns the pass-through off) |
| `sumdb_via` | `database` (default): forward to `https://<name>/...` through Hangar's outbound HTTP settings; `remote`: forward to `<remote_url>/sumdb/<name>/...`, for remotes that proxy the database themselves (Athens, another Hangar) |

`proxy.golang.org` does not serve `sumdb/` paths, so keep the default for it.
The database host must be reachable from Hangar (outbound proxy settings
apply). Hosted repositories answer `404` there, which makes the go command
contact the database directly; private modules belong in `GONOSUMDB` anyway.

## Notes

- Plain HTTP: a `GOPROXY` with `http://` works for repositories with anonymous
  read, but the go command never sends credentials over HTTP (neither from
  `.netrc` nor from the URL). Private repositories need HTTPS.
- Browse tree: module path segments, the module, `@v`, versions, then the
  `.info`, `.mod` and `.zip` of each version. The package view shows the
  README at the module root and the licence found in its `LICENSE` file.
- Vulnerability scanning uses the OSV `Go` ecosystem (module path and version).
