# Helm

Hangar serves classic Helm chart repositories (an `index.yaml` plus chart
archives): `helm repo add`, `helm repo update`, `helm search repo`, `helm pull`,
`helm install`, `helm template` and `helm pull --verify` work against it, and so
does the `helm cm-push` plugin and plain `curl --upload-file`.

OCI charts (`helm push oci://…`, `helm install oci://…`) are not stored here:
push them to a Docker repository, see [docker.md](docker.md).

Placeholders:

- `hangar.example.com`: the host of `server.base_url`.
- `<token>`: a personal access token (`hgr_...`).
- `$HANGAR_TOKEN`: the token in an environment variable.

Repository URL: `https://hangar.example.com/repository/<repo>`.

## Repositories

### Hosted

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "helm-hosted",
        "format": "helm",
        "type": "hosted",
        "blob_store": "default",
        "config": { "hosted": { "write_policy": "allow_once" } }
      }'
```

Write policies, per chart version:

| `write_policy` | Uploading an existing version with other content |
|---|---|
| `allow_once` | refused with `409`, also with `?force` |
| `allow` | refused with `409` unless the upload asks for `?force` (`helm cm-push --force`) |
| `deny` | read-only |

Uploading the exact same archive again is always accepted. An overwrite with
`?force` removes the version's old `.prov` file, which signed the old archive.

### Proxy

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "helm-proxy",
        "format": "helm",
        "type": "proxy",
        "blob_store": "default",
        "config": { "proxy": { "remote_url": "https://charts.example.org/stable", "metadata_ttl": "30m" } }
      }'
```

- `remote_url` is the chart repository URL (what you would give `helm repo add`)
  or the full URL of its `index.yaml`.
- The remote's `index.yaml` is cached for `metadata_ttl` (`0` means 1h), then
  revalidated with `ETag` / `Last-Modified`. While the remote is down the cached
  index is served.
- Entries of the remote's `index.yaml` that do not parse (a malformed date, a
  string where the URL list belongs, an invalid version) are left out; the rest
  of the remote's charts stay available. Each parse logs one warning naming the
  first bad entry and counts the skipped entries in
  `hangar_proxy_index_entries_skipped_total`.
- Every chart URL in the index, relative or absolute, is rewritten to point at
  Hangar; the original URL is kept for fetching. Archives are cached forever and
  checked against the index's `digest`.
- Chart URLs on other hosts are fetched when the host is the remote's own,
  `github.com` (release assets of repositories published on GitHub Pages; the
  redirect to GitHub's download host is followed), or listed in the repository's
  `config.proxy.related_hosts`. Anything else answers `502` with a message naming
  the host to add. The remote's credentials are only sent to the remote's own host.
- `.prov` files are fetched from the chart URL plus `.prov` when a client asks.

### Group

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "helm-group",
        "format": "helm",
        "type": "group",
        "config": { "group": { "members": ["helm-hosted", "helm-proxy"], "write_member": "helm-hosted" } }
      }'
```

The group's `index.yaml` merges the members' entries; when two members have the
same chart name and version, the first member wins. Uploads to the group go to
`write_member` (without one the group is read-only).

## Add the repository

```bash
helm repo add hangar https://hangar.example.com/repository/helm-group \
  --username token --password "$HANGAR_TOKEN"
helm repo update
helm search repo hangar/
```

The username is not checked when the password is a token. Chart URLs in the
index are relative, so Helm sends the credentials with every download (no
`--pass-credentials` needed). For an `http://` server nothing extra is required.

## Install, pull, template

```bash
helm install web hangar/web --version 1.2.0
helm pull hangar/web --version 1.2.0
helm template web hangar/web
```

Without `helm repo add`:

```bash
helm pull web --version 1.2.0 --repo https://hangar.example.com/repository/helm-group \
  --username token --password "$HANGAR_TOKEN"
```

## Upload charts

Package first: `helm package ./web` writes `web-1.2.0.tgz`. Uploads are checked:
the archive must contain `<dir>/Chart.yaml` with `apiVersion` `v1` or `v2`, a
`name` of letters, digits, `.`, `_` and `-`, and a semantic `version`; `type` must
be `application` or `library`.

### helm cm-push

The [helm-push](https://github.com/chartmuseum/helm-push) plugin speaks the
ChartMuseum upload API, which Hangar implements:

```bash
helm plugin install https://github.com/chartmuseum/helm-push
helm repo add hangar https://hangar.example.com/repository/helm-hosted \
  --username token --password "$HANGAR_TOKEN"
helm cm-push web-1.2.0.tgz hangar           # or: helm cm-push ./web hangar
helm cm-push --force web-1.2.0.tgz hangar   # overwrite (write_policy allow only)
```

The plugin reads `serverInfo.contextPath` from `index.yaml` and POSTs to
`<repo>/api/charts`, so no `--context-path` flag is needed. A token can also be
passed as `--access-token "$HANGAR_TOKEN"` (sent as `Authorization: Bearer`).

### curl

```bash
curl -fu "token:$HANGAR_TOKEN" --upload-file web-1.2.0.tgz \
  https://hangar.example.com/repository/helm-hosted/web-1.2.0.tgz
# ChartMuseum style, raw body or multipart field "chart"
curl -fu "token:$HANGAR_TOKEN" --data-binary @web-1.2.0.tgz \
  https://hangar.example.com/repository/helm-hosted/api/charts
```

With `PUT` the file name must be `<name>-<version>.tgz` as in `Chart.yaml`.

### Provenance files

`helm package --sign --key '<name>' --keyring ~/.gnupg/secring.gpg ./web` also
writes `web-1.2.0.tgz.prov`. Upload it after (or with) the chart:

```bash
curl -fu "token:$HANGAR_TOKEN" -F chart=@web-1.2.0.tgz -F prov=@web-1.2.0.tgz.prov \
  https://hangar.example.com/repository/helm-hosted/api/charts
# or on its own
curl -fu "token:$HANGAR_TOKEN" --upload-file web-1.2.0.tgz.prov \
  https://hangar.example.com/repository/helm-hosted/web-1.2.0.tgz.prov
```

Hangar refuses a `.prov` file whose `files:` section does not list the stored
archive's sha256. The signature itself is checked by the client:

```bash
helm pull hangar/web --version 1.2.0 --verify --keyring ~/.gnupg/pubring.gpg
```

## Delete a version

```bash
curl -fu "token:$HANGAR_TOKEN" -X DELETE \
  https://hangar.example.com/repository/helm-hosted/api/charts/web/1.2.0
```

## ChartMuseum read API

For tools and UIs that read a ChartMuseum server, every repository (hosted, proxy
and group) also answers, in JSON:

| Request | Answer |
|---|---|
| `GET <repo>/api/charts` | every chart, each with its versions (newest first) |
| `GET <repo>/api/charts/<name>` | the versions of one chart |
| `GET <repo>/api/charts/<name>/<version>` | one version; `latest` is the highest |

## Layout and permissions

Archives are stored and served at `<repo>/<name>/<version>/<name>-<version>.tgz`
(and `.tgz.prov`), the URLs `index.yaml` lists; `<repo>/<name>-<version>.tgz`
answers too. The **Packages** tree shows chart, version, files; the version page
shows the chart's `README.md`, description, home page, sources, keywords and the
licence from the `artifacthub.io/license` (or `licenses`) annotation.
Maintainers are listed in `index.yaml` and the API, not on the page.

Helm charts have no vulnerability advisory source, so the scanner marks them
"No advisory source".

Permissions: reading the index and charts needs `repo:<name>:read` (or anonymous
read on the repository); uploads need `repo:<name>:write`; delete needs
`repo:<name>:delete`.

## Routing rules

A group that combines a hosted repository with a public proxy can serve a
public package published under one of your internal names. Block internal
names on the proxy and reserve them for the hosted member (for example `edcom-*`);
see [routing-rules.md](routing-rules.md).
