# PyPI: pip, uv, poetry and twine

Hangar serves the Simple Repository API (read by pip, uv and poetry) and the
legacy upload API (used by twine, `uv publish` and `poetry publish`).

Placeholders:

- `hangar.example.com`: the host of `server.base_url`.
- `<token>`: a personal access token (`hgr_...`).
- `$HANGAR_TOKEN`: the token in an environment variable.

URLs of a repository `<repo>`:

| Purpose | URL |
|---|---|
| Index (pip `--index-url`, uv, poetry) | `https://hangar.example.com/repository/<repo>/simple/` |
| Upload (twine, uv, poetry) | `https://hangar.example.com/repository/<repo>/` |

Credentials are HTTP Basic: username `__token__` (any value works) and the token
as the password. Your account username and password also work.

## Repositories

### Hosted

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "pypi-hosted",
        "format": "pypi",
        "type": "hosted",
        "blob_store": "default",
        "config": { "hosted": { "write_policy": "allow_once" } }
      }'
```

With `allow_once`, uploading the same file again succeeds and different bytes
under an existing filename get `409`.

### Proxy (pypi.org)

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "pypi-proxy",
        "format": "pypi",
        "type": "proxy",
        "blob_store": "default",
        "config": {
          "proxy": { "remote_url": "https://pypi.org", "metadata_ttl": "30m" },
          "format": { "file_hosts": ["files.pythonhosted.org"] }
        }
      }'
```

- `remote_url` may be `https://pypi.org` or `https://pypi.org/simple`.
- Project pages are cached for `metadata_ttl` (default `30m`); remote 404s for
  `negative_cache_ttl` (default `5m`).
- Files are verified against the page's sha256 and cached forever.
- `config.format.file_hosts` lists other hosts files may be downloaded from
  (default `["files.pythonhosted.org"]`; `.domain` suffixes allowed). Change it,
  or use `proxy.related_hosts`, for mirrors that serve files from another host.
  The remote's credentials are never sent to those hosts.
- A remote answering `429` for an uncached page or file is passed on as `429`
  with its `Retry-After`. With `"offline": true` in `config.proxy`, only cached
  pages and files are served.

### Group

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "pypi-group",
        "format": "pypi",
        "type": "group",
        "config": { "group": { "members": ["pypi-hosted", "pypi-proxy"], "write_member": "pypi-hosted" } }
      }'
```

Project pages merge the members in order (first member wins per filename).
Every Simple API page carries an `ETag`; pip and uv revalidate their cached
copy and get `304 Not Modified` while the project is unchanged.
Uploads to the group go to `write_member`; without one they get `405`.

Name your internal projects so they cannot collide with pypi.org projects: the
merged page lists every version from every member, and pip picks the highest one
that matches.

## pip

One-off:

```bash
pip install --index-url https://__token__:<token>@hangar.example.com/repository/pypi-group/simple/ requests
```

`pip.conf` (Linux `~/.config/pip/pip.conf`, macOS
`~/Library/Application Support/pip/pip.conf`, Windows `%APPDATA%\pip\pip.ini`, or
any file named by `PIP_CONFIG_FILE`):

```ini
[global]
index-url = https://__token__:<token>@hangar.example.com/repository/pypi-group/simple/
```

To keep the token out of the file, leave the credentials out of the URL and put them
in `~/.netrc`:

```
machine hangar.example.com
login __token__
password <token>
```

Or set `PIP_INDEX_URL` in the environment (handy in CI).

## uv

`pyproject.toml`:

```toml
[[tool.uv.index]]
name = "hangar"
url = "https://hangar.example.com/repository/pypi-group/simple/"
publish-url = "https://hangar.example.com/repository/pypi-hosted/"
default = true    # replaces pypi.org; use explicit = true to only pin selected packages
```

Credentials come from the environment, named after the index:

```bash
export UV_INDEX_HANGAR_USERNAME=__token__
export UV_INDEX_HANGAR_PASSWORD='<token>'
uv lock
uv sync
uv pip install --index-url https://hangar.example.com/repository/pypi-group/simple/ requests
```

The same `[[index]]` table works in `uv.toml` (without the `tool.uv.` prefix).

Publish:

```bash
uv build
export UV_PUBLISH_USERNAME=__token__
export UV_PUBLISH_PASSWORD='<token>'
uv publish --index hangar
# or without an index entry:
uv publish --publish-url https://hangar.example.com/repository/pypi-hosted/
```

## twine

`~/.pypirc`:

```ini
[distutils]
index-servers =
    hangar

[hangar]
repository = https://hangar.example.com/repository/pypi-hosted/
username = __token__
password = <token>
```

```bash
python -m build
twine upload -r hangar dist/*
# or without .pypirc:
twine upload --repository-url https://hangar.example.com/repository/pypi-hosted/ \
  -u __token__ -p "$HANGAR_TOKEN" dist/*
```

twine only honours `--skip-existing` for some public indexes. With `allow_once`,
re-uploading identical files succeeds anyway.

The version in the file name must be the declared version under PEP 440
normalisation: `1.0` matches `1.0.0` and `1.0-1` matches `1.0.post1`, while
`1.0` and `10` are different versions and the upload answers `400`.

## poetry

```bash
poetry source add --priority=primary hangar https://hangar.example.com/repository/pypi-group/simple/
poetry config http-basic.hangar __token__ '<token>'

poetry config repositories.hangar-upload https://hangar.example.com/repository/pypi-hosted/
poetry config http-basic.hangar-upload __token__ '<token>'
poetry publish --build -r hangar-upload
```

In CI, use environment variables instead of `poetry config`:
`POETRY_HTTP_BASIC_HANGAR_USERNAME=__token__` and
`POETRY_HTTP_BASIC_HANGAR_PASSWORD=<token>`.

## Yank a release

Yanked releases stay downloadable for exact pins, but installers skip them
otherwise.

```bash
# yank, with an optional reason
curl -u __token__:"$HANGAR_TOKEN" -X POST \
  https://hangar.example.com/repository/pypi-hosted/yank/acme-tiny/0.1.0 \
  -H 'Content-Type: application/json' -d '{"reason": "broken build"}'

# un-yank
curl -u __token__:"$HANGAR_TOKEN" -X DELETE \
  https://hangar.example.com/repository/pypi-hosted/yank/acme-tiny/0.1.0
```

To delete a release, delete its component through the [REST API](api.md).

## Notes

- Simple pages are served as HTML or PEP 691 JSON
  (`Accept: application/vnd.pypi.simple.v1+json`), with sha256 hashes,
  `requires-python`, yank markers and PEP 658 wheel metadata.
- Requests without credentials to a repository without anonymous read get `401`
  with `WWW-Authenticate: Basic`, so pip falls back to netrc or keyring.
- Plain HTTP: pip ignores an `http://` index on any host other than localhost
  unless the host is listed in `--trusted-host` (or `trusted-host` in `pip.conf`).

## Routing rules

A group that combines a hosted repository with a public proxy can serve a
public package published under one of your internal names. Block internal
names on the proxy and reserve them for the hosted member (for example `edcom-*`);
see [routing-rules.md](routing-rules.md).
