# npm, pnpm and yarn

Hangar implements the npm registry protocol. npm, pnpm, yarn classic and yarn
berry install from it; npm (and yarn berry's `yarn npm publish`) publish to it.

Placeholders:

- `hangar.example.com`: the host of `server.base_url`.
- `<token>`: a personal access token (`hgr_...`).
- `$HANGAR_TOKEN`: the token in an environment variable.

Registry URL of a repository: `https://hangar.example.com/repository/<repo>/`
(the trailing slash matters).

## Repositories

### Hosted

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "npm-hosted",
        "format": "npm",
        "type": "hosted",
        "blob_store": "default",
        "config": {
          "hosted": { "write_policy": "allow_once" },
          "format": { "unpublish_window": "72h", "login_token_ttl": "720h" }
        }
      }'
```

`config.format` is optional:

| Key | Default | Meaning |
|---|---|---|
| `unpublish_window` | `72h` | how long after publishing a user with `repo:<name>:delete` may unpublish; `0s` = only `repo:<name>:admin` |
| `login_token_ttl` | `720h` | lifetime of tokens issued by `npm login` |
| `reuse_unpublished_versions` | `false` | allow publishing a version number again after it was unpublished |

Published versions are always immutable (`403`, as on the public registry), whatever
the write policy. `write_policy: deny` refuses publishing.

### Proxy (registry.npmjs.org)

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "npm-proxy",
        "format": "npm",
        "type": "proxy",
        "blob_store": "default",
        "config": { "proxy": { "remote_url": "https://registry.npmjs.org", "metadata_ttl": "30m" } }
      }'
```

Package documents are cached for `metadata_ttl`, then revalidated; a stale copy is
served while the remote is down. Tarballs are verified against the upstream
checksums and cached. Tarball URLs in the documents point at Hangar, so lockfiles
record Hangar URLs.

Tarball URLs follow the public registry's layout in every repository type:
`<repository>/@scope/name/-/name-1.0.0.tgz` for scoped packages, also when the
remote names them `@scope/name/-/@scope/name-1.0.0.tgz` (some self-hosted
registries do). That older form is still answered, so lockfiles that recorded
it keep installing; npm, pnpm, yarn and update bots such as Renovate resolve
the conventional one.

### Group

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "npm-group",
        "format": "npm",
        "type": "group",
        "config": { "group": { "members": ["npm-hosted", "npm-proxy"], "write_member": "npm-hosted" } }
      }'
```

Package documents of the members are merged in member order: the first member that
has a version or dist-tag wins. Publishing, dist-tags, deprecate and unpublish sent
to the group go to `write_member`; without one the group is read-only (`405`).

With a group, one registry URL serves both your packages and the public ones.

## npm

### `.npmrc`

Put this in the project's `.npmrc` (or `~/.npmrc`). The auth line is keyed by host
and path, without the scheme, and must end with `/`:

```ini
registry=https://hangar.example.com/repository/npm-group/
//hangar.example.com/repository/npm-group/:_authToken=${HANGAR_TOKEN}
```

npm expands `${HANGAR_TOKEN}` from the environment, so the file can be committed
without the secret. Add one auth line per repository URL you use (for example
`npm-hosted` if you publish to it directly).

Only route one scope through Hangar and keep the public registry for the rest:

```ini
@acme:registry=https://hangar.example.com/repository/npm-hosted/
//hangar.example.com/repository/npm-hosted/:_authToken=${HANGAR_TOKEN}
```

### Install and publish

```bash
npm install
npm publish                      # goes to registry=..., here the group's write member
npm publish --registry https://hangar.example.com/repository/npm-hosted/
```

Or set the target in `package.json`:

```json
{ "publishConfig": { "registry": "https://hangar.example.com/repository/npm-hosted/" } }
```

### Dist-tags, deprecate, whoami

```bash
npm dist-tag add @acme/greeting@1.0.0 stable
npm dist-tag ls @acme/greeting
npm dist-tag rm @acme/greeting stable          # "latest" cannot be removed
npm deprecate @acme/greeting@1.0.0 "use 1.1.0"
npm deprecate @acme/greeting@1.0.0 ""          # undeprecate
npm whoami
```

### Unpublish

```bash
npm unpublish @acme/greeting@1.0.0 --registry https://hangar.example.com/repository/npm-hosted/
npm unpublish @acme/greeting --force --registry https://hangar.example.com/repository/npm-hosted/
```

- Needs `repo:<name>:delete` within `unpublish_window` of the publish, or
  `repo:<name>:admin` at any time. Unpublishing also rewrites the package state
  (dist-tags and tombstones), which needs `repo:<name>:write`.
- An unpublished version leaves a tombstone: publishing the same version number
  again is refused with 403, so a version always means the same bytes for every
  lockfile. Publish a new version instead, or set
  `config.format.reuse_unpublished_versions: true` on the repository to allow
  reuse. Tombstones are written by the npm unpublish protocol; components
  deleted through the REST API or cleanup do not leave one.

### `npm login`

`npm login --registry https://hangar.example.com/repository/npm-hosted/` asks for
your username and password (a token also works as the password) and stores a
token in `~/.npmrc`. Web login is not offered, so npm falls back to the
username/password prompt. `npm login` does not create users (`npm adduser` for an
unknown user fails). The issued token lives `login_token_ttl`. In CI, write a
token into `.npmrc` instead.

`npm logout` revokes the token `npm login` issued. A personal access token
written into `.npmrc` is left alone (npm only forgets its copy); revoke it under
**Access tokens**. Tokens issued by `npm login` before this release are not
recognised as such and expire after `login_token_ttl`.

### `npm audit`

`npm audit` (and `pnpm audit`, `yarn npm audit`) get real answers:

- **Proxy repositories** pass the audit to their remote's advisory endpoint
  (registry.npmjs.org's for the public registry). Only package names the
  proxy's routing rules allow are sent; names a rule keeps private never leave
  Hangar.
- **Hosted repositories** answer from Hangar's own vulnerability findings (OSV
  scanning, see [vulnerabilities.md](vulnerabilities.md)) for the versions they
  store. Waived findings are not reported; advisories without a severity count
  as `high`, as npm treats them.
- **Groups** combine both: each package is asked of the members the group's
  routing rules let serve it, and the answers are merged (the same advisory
  from both sources is listed once).

Hangar never answers "no advisories" when it cannot vouch for that. The audit
fails, and npm prints `audit endpoint returned an error`, when:

- a hosted version in the request has no successful scan yet (not scanned,
  or the last scan failed; the message names them). Turn on
  `scan_on_arrival` for hosted npm repositories, or scan them from the
  repository page;
- vulnerability scanning is turned off (`scanning.enabled: false`) and the
  request names a version Hangar stores (503);
- a proxy's remote cannot answer the audit (502).

### `npm search`

`npm search <text>` works on every repository type: hosted repositories search
their packages (name, scope, description), proxies ask their remote's search
(results their routing rules exclude are dropped), and groups merge their
members' results in member order. `size` is capped at 250.

## pnpm

pnpm reads the same `.npmrc`:

```bash
pnpm install
pnpm publish --registry https://hangar.example.com/repository/npm-hosted/
```

pnpm 11 and later hold back versions younger than `minimum-release-age`. A package
you published minutes ago may not resolve until then; for internal packages, set
`minimum-release-age=0` in `.npmrc` or pass `--config.minimum-release-age=0`.

## yarn berry (2+)

`.yarnrc.yml` in the project:

```yaml
npmRegistryServer: "https://hangar.example.com/repository/npm-group/"
npmAlwaysAuth: true
npmAuthToken: "${HANGAR_TOKEN}"
```

For a single scope:

```yaml
npmScopes:
  acme:
    npmRegistryServer: "https://hangar.example.com/repository/npm-hosted/"
    npmAlwaysAuth: true
    npmAuthToken: "${HANGAR_TOKEN}"
```

```bash
yarn install
yarn npm publish
```

Over plain HTTP, yarn berry also needs the host in `unsafeHttpWhitelist`:

```yaml
unsafeHttpWhitelist:
  - "hangar.example.com"
```

## yarn classic (1.x)

yarn 1 reads `.npmrc`. It only sends credentials for unscoped packages when
`always-auth` is set:

```ini
registry=https://hangar.example.com/repository/npm-group/
//hangar.example.com/repository/npm-group/:_authToken=${HANGAR_TOKEN}
always-auth=true
```

## Not supported yet

`npm access`, `npm owner`, teams and provenance attestations
(accepted on publish but not stored).

## Proxy tarball names

A remote whose tarball URLs do not end in `.tgz` (a download endpoint such as
`https://cdn.example.com/download/lodash/4.17.21`) is served too: Hangar
advertises such tarballs under the conventional name
(`lodash/-/lodash-4.17.21.tgz`) and fetches them from the remote's URL. A URL
on another host than the remote needs that host in the proxy's
`related_hosts`.

## Routing rules

A group that combines a hosted repository with a public proxy can serve a
public package published under one of your internal names. Block internal
names on the proxy and reserve them for the hosted member (for example `@trk/*`);
see [routing-rules.md](routing-rules.md).
