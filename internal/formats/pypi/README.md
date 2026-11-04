# `pypi` format

Implements the **PyPI** format: the Simple Repository API read by pip, uv and
poetry, and the legacy upload API used by twine. Protocol requirements and the
real-client acceptance checklist are in [docs/FORMATS.md](../../../docs/FORMATS.md),
section "PyPI".

Specs: [Simple repository API](https://packaging.python.org/en/latest/specifications/simple-repository-api/)
(PEP 503, 592, 629, 658, 691, 700, 714) and the
[upload API](https://docs.pypi.org/api/upload/).

## Client configuration

```
pip install --index-url https://hangar.example.com/repository/pypi-group/simple/ <pkg>
uv pip install --index-url https://hangar.example.com/repository/pypi-group/simple/ <pkg>
twine upload --repository-url https://hangar.example.com/repository/pypi-hosted/ dist/*
```

Credentials: HTTP Basic (username + password, or any username + a `hgr_` token).
Unauthenticated requests to a repository without anonymous read get
`401` with `WWW-Authenticate: Basic`, so pip falls back to netrc/keyring.

## Routes (relative to `/repository/{repo}/`)

| Method | Path | |
|---|---|---|
| GET | `simple/` | project list |
| GET | `simple/{project}/` | project page; non-normalised names `301` to the normalised URL, missing slashes `301` |
| GET, HEAD | `packages/{project}/{filename}` | distribution file (Range supported) |
| GET, HEAD | `packages/{project}/{filename}.metadata` | core metadata of a wheel (PEP 658) |
| POST | `/` or `legacy/` | upload, `multipart/form-data` with `:action=file_upload` |
| POST | `yank/{project}/{version}` | yank a release; optional body `{"reason": "..."}` |
| DELETE | `yank/{project}/{version}` | un-yank a release |

Simple API pages are negotiated from `Accept` (or `?format=`):
`application/vnd.pypi.simple.v1+json`, `application/vnd.pypi.simple.v1+html`,
`text/html` (also for `*/*` and no `Accept`); anything else is `406`. Pages
declare API version 1.1 (sizes, upload times, versions) and carry `#sha256=`
link hashes, `data-requires-python`, `data-yanked`, and
`data-core-metadata` / `data-dist-info-metadata` (JSON: `core-metadata`,
`dist-info-metadata`). All links are absolute and built from `server.base_url`.

## Storage

- **Hosted**: one component per release (namespace empty, name PEP 503
  normalised, attributes `display_name`, `summary`, `requires_python`, `yanked`,
  `yanked_reason`, `metadata` = the upload's core metadata fields). One content
  asset per file at `packages/{project}/{filename}` (checksums sha256, md5,
  blake2b_256); wheels also get a metadata asset `….whl.metadata`, extracted from
  the stored wheel with random access (never buffered).
- Uploads stream to the blob store; the declared `sha256_digest`,
  `md5_digest` (hex or urlsafe base64) and `blake2_256_digest` are verified, at
  least one is required. The filename must belong to `name`/`version` and match
  `filetype` (`sdist` or `bdist_wheel`). The write policy applies: under
  `allow_once` the same bytes are accepted again, different bytes get `409`
  (twine `--skip-existing` treats that as "exists").
- **Proxy**: project pages are fetched from the remote (`remote_url` may be
  `https://pypi.org` or `https://pypi.org/simple`) as PEP 691 JSON, falling back
  to HTML, and cached at `simple/{project}/index.json` for
  `proxy.metadata_ttl` (default 30m), then revalidated with ETag /
  Last-Modified; a stale copy is served when the remote fails. Remote 404s are
  remembered by the core upstream for `proxy.negative_cache_ttl` (default 5m;
  `POST /api/v1/repositories/{name}/cache:invalidate` forgets them). Files
  are downloaded on first request, verified against the page's sha256 (or
  md5), cached at the same `packages/` paths and never re-fetched. All
  downloads go through the repository's upstream (SSRF guard, circuit
  breaker, request collapsing, outbound proxy, Retry-After handling, resumed
  transfers). Files on other hosts are fetched only if the host is in
  `config.format.file_hosts` (default `["files.pythonhosted.org"]`, `.domain`
  suffixes allowed) or `proxy.related_hosts`, without the remote's
  credentials. A remote answering 429 for an uncached page or file is passed
  on as 429 with its `Retry-After`; in `proxy.offline` mode only cached
  pages and files are served. Cache fills are published as the reading principal
  (the component service authorizes `repo:<proxy>:read` for proxy publishes,
  or `repo:<group>:read` when the read came through a group).
- **Group**: project pages merge members in order, first member wins per
  filename; links point at the group; downloads resolve members in order.
  Uploads to a group go to `group.write_member` (hosted pypi) if set, else `405`.
- Quarantined components are hidden from project pages and their files answer `403`.

## Format config

```yaml
config:
  format:
    file_hosts: [files.pythonhosted.org]   # proxy only
```

## Tests

- Unit tests (`go test ./internal/formats/pypi/`) use the `coretest` fakes.
- `TestRealClients` builds a wheel and sdist with `python -m build`, uploads
  them with twine, installs/downloads through a hosted + pypi.org proxy group
  with pip and uv. It creates a venv in the test's temp dir and needs network
  access to pypi.org; it skips otherwise, and with `-short`.
- Acceptance against a running server: `test/e2e/pypi_test.go` (build tag `e2e`).
