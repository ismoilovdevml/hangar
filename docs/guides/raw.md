# Raw files

A raw repository stores arbitrary files by path: build outputs, installers,
tarballs, firmware, scripts. Any HTTP client works; this guide uses curl and wget.

Placeholders:

- `hangar.example.com`: the host of `server.base_url`.
- `<user>`: your username (any value works when the password is a token).
- `<token>`: a personal access token (`hgr_...`).
- `$HANGAR_TOKEN`: the token in an environment variable.

File URL: `https://hangar.example.com/repository/<repo>/<path>`.

## Repositories

### Hosted

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "raw-hosted",
        "format": "raw",
        "type": "hosted",
        "blob_store": "default",
        "config": { "hosted": { "write_policy": "allow" } }
      }'
```

Write policies: `allow` (overwrite freely), `allow_once` (identical re-uploads are
accepted, changed content is `409`), `deny` (read-only). Add
`"anonymous_read": true` to `config` to serve downloads without credentials.

### Proxy

Any HTTP server can be the remote; paths are appended to `remote_url`:

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "raw-proxy",
        "format": "raw",
        "type": "proxy",
        "blob_store": "default",
        "config": { "proxy": { "remote_url": "https://downloads.example.org/", "content_ttl": "24h", "negative_cache_ttl": "5m" } }
      }'
```

- `content_ttl`: `0` trusts cached files forever; otherwise a cached file older than
  this is revalidated with the remote.
- `negative_cache_ttl`: how long a remote 404 is remembered.
- When the remote fails, a cached copy is still served.

### Group

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "raw-group",
        "format": "raw",
        "type": "group",
        "config": { "group": { "members": ["raw-hosted", "raw-proxy"] } }
      }'
```

A file comes from the first member that has it; listings merge all members. Raw
groups are read-only (upload to the hosted repository).

## Authentication

Either form works for every request below:

```bash
curl -u <user>:"$HANGAR_TOKEN" ...
curl -H "Authorization: Bearer $HANGAR_TOKEN" ...
```

Unauthenticated requests that need credentials get `401` with
`WWW-Authenticate: Basic`, which `wget` needs before it sends credentials.

## Upload

```bash
curl -u <user>:"$HANGAR_TOKEN" --upload-file app-1.0.tar.gz \
  https://hangar.example.com/repository/raw-hosted/tools/1.0/app-1.0.tar.gz

# same thing; with a trailing slash curl appends the local file name
curl -u <user>:"$HANGAR_TOKEN" -T app-1.0.tar.gz \
  https://hangar.example.com/repository/raw-hosted/tools/1.0/
```

- `201 Created` for a new file, `200` when it replaced one. The response body
  describes the stored file (size, content type, md5/sha1/sha256/sha512).
- Optional integrity check: send `X-Checksum-Sha256`, `X-Checksum-Sha1`,
  `X-Checksum-Md5` (hex) or `Content-MD5` (base64). On mismatch the upload is
  rejected with `400` and nothing is stored:

  ```bash
  curl -u <user>:"$HANGAR_TOKEN" -T app-1.0.tar.gz \
    -H "X-Checksum-Sha256: $(sha256sum app-1.0.tar.gz | cut -d' ' -f1)" \
    https://hangar.example.com/repository/raw-hosted/tools/1.0/app-1.0.tar.gz
  ```

- `Content-Type` is stored as sent; when missing it is guessed from the extension.
- A file cannot be written where a directory exists, or below an existing file
  (`409`).

With wget:

```bash
wget --method=PUT --body-file=app-1.0.tar.gz --user <user> --password "$HANGAR_TOKEN" \
  https://hangar.example.com/repository/raw-hosted/tools/1.0/app-1.0.tar.gz
```

## Download

```bash
curl -u <user>:"$HANGAR_TOKEN" -fLO https://hangar.example.com/repository/raw-hosted/tools/1.0/app-1.0.tar.gz
wget --user <user> --password "$HANGAR_TOKEN" https://hangar.example.com/repository/raw-group/tools/1.0/app-1.0.tar.gz
```

- `Range` requests (`curl -C -`, resumed downloads) and conditional requests
  (`If-None-Match`, `If-Modified-Since`) are supported. The `ETag` is the quoted
  sha256.
- Responses carry `X-Checksum-Sha256`, `X-Checksum-Sha1` and `X-Checksum-Md5`.
- Every file is served as an attachment (`Content-Disposition`), so uploaded HTML
  never renders in the registry's origin.

## Directory listings

A path ending in `/` (or the repository root) lists the entries directly below it.
Browsers get HTML; ask for JSON with the `Accept` header:

```bash
curl -u <user>:"$HANGAR_TOKEN" -H 'Accept: application/json' \
  https://hangar.example.com/repository/raw-hosted/tools/1.0/
```

```json
{
  "repository": "raw-hosted",
  "path": "tools/1.0/",
  "entries": [
    {"name": "app-1.0.tar.gz", "path": "tools/1.0/app-1.0.tar.gz", "type": "file", "size": 1048576,
     "content_type": "...", "last_modified": "2026-11-04T06:04:04Z",
     "checksums": {"md5": "...", "sha1": "...", "sha256": "...", "sha512": "..."}}
  ]
}
```

Page with `?limit=` (default 50 for JSON, 1000 for HTML, max 1000) and the
returned `next_cursor` (`?cursor=`). A directory with nothing below it is `404`.

Mirror a directory tree with wget over the HTML listing:

```bash
wget -r -np -nH --cut-dirs=2 -e robots=off --reject-regex '\?cursor=' \
  --user <user> --password "$HANGAR_TOKEN" \
  https://hangar.example.com/repository/raw-hosted/tools/
```

## Delete

```bash
curl -u <user>:"$HANGAR_TOKEN" -X DELETE \
  https://hangar.example.com/repository/raw-hosted/tools/1.0/app-1.0.tar.gz
```

`204` on success; needs `repo:<name>:delete`. On a proxy repository, DELETE evicts
the cached copy. Groups do not accept DELETE.

## Paths

`.` and `..` segments, backslashes, control characters and invalid UTF-8 are
rejected with `400`; empty segments collapse (`a//b` is `a/b`). Segments are
limited to 255 bytes, paths to 4096 bytes.
