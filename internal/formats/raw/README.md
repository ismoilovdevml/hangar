# `raw` format

Stores and serves arbitrary files by path. Protocol requirements and the
real-client acceptance checklist are in [docs/FORMATS.md](../../../docs/FORMATS.md),
section "Raw".

## Protocol

All routes live under `/repository/{repo}/`.

| Request | Hosted | Proxy | Group |
|---|---|---|---|
| `PUT {path}` | store the body (201 new, 200 replaced) | 405 | 405 |
| `GET`/`HEAD {path}` | serve the file | cached copy, fetched from the remote on a miss | first member that has it |
| `DELETE {path}` | remove the file (204) | evict the cached copy | 405 |
| `GET {dir}/` | directory listing | listing of cached files | merged listing of all members |

Privileges: `repo:<name>:read` for GET/HEAD and listings, `write` for PUT,
`delete` for DELETE. Unauthenticated requests that need a login get
`401` with `WWW-Authenticate: Basic realm="Hangar"`, so `wget` (which only sends
credentials after a challenge) and `curl -u` both work. A token works as the
Basic password or as `Authorization: Bearer`.

### Uploads

- The body is streamed into the blob store; md5, sha1, sha256 and sha512 are
  computed on the way and stored as the asset's checksums.
- Optional verification: `X-Checksum-Sha256`, `X-Checksum-Sha1`,
  `X-Checksum-Md5` (hex) and `Content-MD5` (base64, RFC 1864). A mismatch
  answers 400 and nothing is published.
- `Content-Type` is stored as sent (normalised). When absent, or a form
  encoding that clients add by default, it is guessed from the extension,
  falling back to `application/octet-stream`.
- The tree stays a tree: a file cannot be written where a directory exists or
  below an existing file (409).
- The repository write policy applies (`allow_once`: identical re-uploads are
  accepted, changed content is 409; `deny`: 409).
- The response is the file's JSON description, with `ETag` and `Location`.

### Downloads

- `ETag` is the quoted sha256; `Last-Modified` is the last write time (the
  remote's `Last-Modified` for proxied content). `Range`, `If-Range`,
  `If-None-Match` and `If-Modified-Since` are answered by `http.ServeContent`.
- `X-Checksum-Sha256`/`-Sha1`/`-Md5` carry the stored checksums.
- Every file is served with `Content-Disposition: attachment; filename=...`,
  `X-Content-Type-Options: nosniff` and `Content-Security-Policy: default-src
  'none'; sandbox`: uploaded HTML or SVG must never run in the registry origin.
- Only complete `200` GETs count as downloads.
- A file path that is really a directory redirects (302) to `dir/`.

### Directory listings

A path that ends in `/` (or the repository root) lists the files and
subdirectories directly below it, sorted by byte order of the name
(directories compare with their trailing `/`). Browsers get HTML; a request
whose `Accept` prefers `application/json` gets:

```json
{
  "repository": "raw-hosted",
  "path": "tools/",
  "entries": [
    {"name": "1.0", "path": "tools/1.0/", "type": "directory"},
    {"name": "README.md", "path": "tools/README.md", "type": "file", "size": 120,
     "content_type": "text/markdown", "last_modified": "2026-11-03T12:00:00Z",
     "checksums": {"md5": "...", "sha1": "...", "sha256": "...", "sha512": "..."}}
  ],
  "next_cursor": "..."
}
```

`?limit=` (default 50 for JSON, 1000 for HTML, max 1000) and `?cursor=` page
through it. A directory with nothing below it is 404 (the root is always 200).

The asset store has no directory objects: a listing scans the assets under the
prefix in path order and collapses deeper paths into their first segment. The
cursor holds the last name emitted and each member's store cursor, so a page
costs at most one extra store page per member. A page stops early (with a
cursor) after scanning 20 000 assets, so a huge subtree cannot make one request
unbounded.

### Paths

The path is decoded once and then validated: `.` and `..` segments,
backslashes, NUL and other control characters, invalid UTF-8, segments over
255 bytes and paths over 4096 bytes are rejected with 400, as are segments that
still decode to any of these after up to three further percent-decodings
(`%252e%252e`). Empty segments collapse (`a//b` is `a/b`). A literal `%`
that is not such an escape (`100%.txt`) is a valid name.

## Component model

Each file is one component with exactly one asset:

- namespace: the directory path (`""` at the root), e.g. `tools/1.0`
- name: the file name, e.g. `tool-1.0.tar.gz`
- version: empty

`Coordinates` returns the same. DELETE removes the component (and with it the
asset) and publishes `component.deleted`; PUT publishes `component.published`.

## Proxy

- Any path is fetched from `remote_url` + path (an arbitrary HTTP server) and
  cached through `ComponentService.Publish`.
- `content_ttl`: 0 trusts cached files forever; otherwise a file older than the
  TTL is revalidated with `If-None-Match`/`If-Modified-Since` (the remote's
  validators are kept in the asset attributes `upstream_etag`,
  `upstream_last_modified`; `cached_at` records the last confirmation).
- `negative_cache_ttl`: remote 404s are remembered in memory for that long
  (at most 10 000 entries per node).
- When the remote fails, or drops a file, a cached copy is still served; an
  uncached file is 502 (unreachable/5xx) or 404.
- Concurrent misses for one path download once (singleflight). The download
  completes even if the first client disconnects (bounded at one hour).
- Cache fills publish as the requesting principal; `ComponentService`
  authorizes a proxy publish as a read, so readers need no write permission.
  A read through a group fills a proxy member's cache on the group's
  `repo:<group>:read` (the outermost group the client addressed).

## Group

Members are expanded depth-first (nested groups allowed up to 8 levels,
repeated, offline and non-raw members skipped). A file comes from the first
member that has it; a proxy member that fails is skipped, and if no member has
the file the answer is 502 instead of 404. Listings merge all members; on a
name clash the first member's file is shown.

## Tests

- Unit tests (`go test ./internal/formats/raw/`) use the in-memory fakes in
  `internal/core/coretest` through `formats.NewHandler`.
- Real clients: `client_test.go` runs `curl` (`--upload-file`, `-T dir/`,
  `-O`, `-J -O`, `-C -`, `--path-as-is`) and `wget` (download, challenge auth,
  `--content-disposition`, `-r` mirror over the HTML listing, `--method=PUT`)
  against an `httptest.Server`; each skips when the binary is missing.
- Fuzzing: `go test -fuzz FuzzNormalizePath ./internal/formats/raw/`.
- Acceptance against a running server: `test/e2e/raw_test.go` (build tag
  `e2e`; needs `HANGAR_E2E_URL`, `HANGAR_E2E_USER`, `HANGAR_E2E_PASSWORD`).
