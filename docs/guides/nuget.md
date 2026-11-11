# NuGet

Hangar implements the NuGet V3 protocol: `dotnet restore`, `dotnet add package`,
`dotnet nuget push`, `dotnet nuget delete` and `dotnet package search` work against
it.

Placeholders:

- `hangar.example.com`: the host of `server.base_url`.
- `<token>`: a personal access token (`hgr_...`).
- `$HANGAR_TOKEN`: the token in an environment variable.

Package source URL (service index): `https://hangar.example.com/repository/<repo>/index.json`.

## Repositories

### Hosted

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "nuget-hosted",
        "format": "nuget",
        "type": "hosted",
        "blob_store": "default",
        "config": {
          "hosted": { "write_policy": "allow_once" },
          "format": { "delete_mode": "unlist" }
        }
      }'
```

`config.format.delete_mode`: `unlist` (default; `dotnet nuget delete` hides the
version from search but keeps it restorable) or `delete` (removes it). With
`allow_once` (the default for repositories created without a policy) or `deny`,
pushing an id and version that already exist gets `409`, also with identical
bytes, as on nuget.org; `dotnet nuget push --skip-duplicate` skips it. With
`allow` the package is replaced and keeps its listed or unlisted state.

### Proxy (nuget.org)

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "nuget-proxy",
        "format": "nuget",
        "type": "proxy",
        "blob_store": "default",
        "config": { "proxy": { "remote_url": "https://api.nuget.org", "metadata_ttl": "1h" } }
      }'
```

`remote_url` is the feed base, not the service index. The service index path
defaults to `v3/index.json`; for another feed layout set
`config.format.service_index` (a path relative to `remote_url`). Metadata is cached
for `metadata_ttl` (`0` means 1h); packages are cached forever. Hosts the
service index points to outside `remote_url` (nuget.org's search service) are
fetched as related hosts, without the remote's credentials. When search there
fails, is rate limited or the proxy is `offline`, search answers from the cached
packages.

#### Feeds that need a key

Some private feeds take their key in the URL path (`https://feed.example.com/<feed-key>/api/v3/index.json`):
configured as `remote_url: https://feed.example.com` and
`service_index: /<feed-key>/api/v3/index.json`. Hangar treats `service_index`
as sensitive: it is returned only to administrators of the repository
(`repo:<name>:admin` or `system:repos:admin`); everyone else, the YAML export
included, sees `***`, which shows that a custom index is set. An update that
sends `***` back keeps the stored value. Error messages sent to clients name
it `<service_index>`.

The path is still stored in the repository configuration as plain text,
unlike `config.proxy.auth`, whose secrets are encrypted at rest and never
returned by the API. Where the feed accepts the key as a credential (HTTP
Basic or a header; check the feed's own documentation), put it in
`config.proxy.auth` and keep `service_index` free of secrets:

```json
"config": {
  "proxy": {
    "remote_url": "https://feed.example.com",
    "auth": { "kind": "basic", "username": "<user>", "password": "<feed-key>" }
  }
}
```

### Group

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "nuget-group",
        "format": "nuget",
        "type": "group",
        "config": { "group": { "members": ["nuget-hosted", "nuget-proxy"], "write_member": "nuget-hosted" } }
      }'
```

Versions, registrations and search results are merged across members (first
member wins per version). Pushes to the group go to `write_member`.

## Configure a source

### `nuget.config`

Place it next to the solution (NuGet merges config files from the project
directory upwards):

```xml
<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <packageSources>
    <clear />
    <add key="hangar" value="https://hangar.example.com/repository/nuget-group/index.json" />
  </packageSources>
  <packageSourceCredentials>
    <hangar>
      <add key="Username" value="token" />
      <add key="ClearTextPassword" value="%HANGAR_TOKEN%" />
    </hangar>
  </packageSourceCredentials>
</configuration>
```

- `%HANGAR_TOKEN%` is expanded from the environment, so the file can be
  committed. The username is not checked when the password is a token.
- `<clear />` makes Hangar the only source; restore of public packages then goes
  through the group's nuget.org proxy.
- The service index is public; every other request needs credentials unless the
  repository allows anonymous read. A request that sends a wrong, expired or
  revoked token answers `401 (Unauthorized)`, the service index included.

### `dotnet nuget add source`

```bash
dotnet nuget add source https://hangar.example.com/repository/nuget-group/index.json \
  --name hangar --username token --password '<token>' --store-password-in-clear-text
```

`--store-password-in-clear-text` is required on macOS and Linux, where the
password cannot be encrypted.

### Plain HTTP

Recent .NET SDKs refuse `http://` sources unless allowed per source:

```xml
<add key="hangar" value="http://hangar.example.com:8080/repository/nuget-group/index.json"
     allowInsecureConnections="true" />
```

`dotnet nuget add source` takes `--allow-insecure-connections` for the same
setting, and `dotnet nuget push` accepts `--allow-insecure-connections` too.

## Restore and add packages

```bash
export HANGAR_TOKEN='<token>'
dotnet add package Acme.Tiny
dotnet restore
dotnet package search Acme --source hangar
```

## Push

`dotnet nuget push` sends the API key in the `X-NuGet-ApiKey` header; use a token as
the key:

```bash
dotnet pack -c Release -o out
dotnet nuget push out/Acme.Tiny.1.0.0.nupkg \
  -s https://hangar.example.com/repository/nuget-hosted/index.json -k "$HANGAR_TOKEN"
```

Pushing to the group URL stores the package in its `write_member`. Symbol packages
(`.snupkg`) are accepted and stored next to the package.

## Unlist, relist, delete

```bash
# source name from nuget.config, pointing at the hosted repository
dotnet nuget delete Acme.Tiny 1.0.0 -s hangar-hosted -k "$HANGAR_TOKEN" --non-interactive
```

With `delete_mode: unlist` this unlists the version. `dotnet nuget delete` has no
`--allow-insecure-connections` flag; for an `http://` server, reference a named
source that has `allowInsecureConnections="true"`. To relist, send
`POST https://hangar.example.com/repository/nuget-hosted/api/v2/package/Acme.Tiny/1.0.0`
with the token in `X-NuGet-ApiKey`.

Permissions: push needs `repo:<name>:write`, unlist needs `delete` and `write`,
relist needs `write`.

## Quarantine and NuGet Audit

A version quarantined by a security policy is left out of the version list,
the registration and search of the repository and of the groups containing it,
so `dotnet` resolves floating versions and ranges to a version it can
download; its `.nupkg` answers `403`.

Every repository advertises a `VulnerabilityInfo/6.7.0` resource, which NuGet
Audit reads during `dotnet restore` (warnings `NU1901` to `NU1904`):

- a hosted repository serves the open findings of this server's vulnerability
  scans on its packages (only when scanning is enabled; waived and fixed
  findings are left out);
- a proxy passes the remote's vulnerability pages through, cached for the
  metadata TTL (nuget.org publishes them);
- a group lists the pages of its members.

Audit settings are the client's: `<NuGetAudit>`, `<NuGetAuditMode>all</NuGetAuditMode>`
for transitive packages, `<NuGetAuditLevel>`.

## Routing rules

A group that combines a hosted repository with a public proxy can serve a
public package published under one of your internal names. Block internal
names on the proxy and reserve them for the hosted member (for example `Edcom.*`);
see [routing-rules.md](routing-rules.md).
