# Configuration as code

`hangar apply`, `hangar diff` and `hangar export` manage a Hangar server's
configuration from YAML files kept in Git: repositories (hosted, proxy and
group, every format, routing rules included), blob stores, cleanup policies,
roles and outbound proxies. They are subcommands of the `hangar` binary and
talk to a running server over the REST API; they need no database access.

```sh
export HANGAR_URL=https://hangar.example.com
export HANGAR_TOKEN=hgr_...            # or --token-file /run/secrets/hangar-token
hangar diff  -f hangar/ --apply-set platform
hangar apply -f hangar/ --apply-set platform --prune
```

## Resource documents

Each resource is one YAML document; a file may hold several, separated by
`---`. The `spec` is the request body of the REST API for that resource
(`api/openapi.yaml`) without its name, which is `metadata.name`:

| `kind` | REST collection | `spec` schema |
|---|---|---|
| `OutboundProxy` | `/proxies` | `OutboundProxyInput` |
| `BlobStore` | `/blob-stores` | `BlobStoreInput` |
| `Role` | `/roles` | `RoleInput` |
| `Repository` | `/repositories` | `RepositoryInput` |
| `CleanupPolicy` | `/cleanup-policies` | `CleanupPolicyInput` (apply previews the policy and sends `preview_token` itself) |

Routing rules are `config.routing` of the proxy or group repository they
protect (see [routing-rules.md](routing-rules.md)); they are not a kind of
their own.

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/ismoilovdevml/hangar/main/api/hangar-config.schema.json
apiVersion: hangar/v1
kind: BlobStore
metadata:
  name: s3-main
spec:
  kind: s3
  s3:
    endpoint: https://s3.eu-central-1.amazonaws.com
    region: eu-central-1
    bucket: acme-hangar
    access_key_id: AKIA...
    secret_access_key: {valueFrom: {env: HANGAR_S3_SECRET}}
---
apiVersion: hangar/v1
kind: Repository
metadata:
  name: npm-hosted
  labels:
    team: web
spec:
  format: npm
  type: hosted
  blob_store: s3-main
  description: Internal packages
  config:
    hosted:
      write_policy: allow_once
---
apiVersion: hangar/v1
kind: Repository
metadata:
  name: npm-proxy
spec:
  format: npm
  type: proxy
  blob_store: s3-main
  config:
    proxy:
      remote_url: https://registry.npmjs.org
      metadata_ttl: 30m
      auth:
        kind: bearer
        token: {valueFrom: {file: secrets/npm-token}}
    routing:
      block: ["@acme/*"]
---
apiVersion: hangar/v1
kind: Repository
metadata:
  name: npm
spec:
  format: npm
  type: group
  config:
    group:
      members: [npm-hosted, npm-proxy]
      write_member: npm-hosted
---
apiVersion: hangar/v1
kind: Role
metadata:
  name: ci-publisher
spec:
  description: CI pipelines publishing npm packages
  privileges: ["repo:npm-hosted:write", "repo:*:read"]
---
apiVersion: hangar/v1
kind: CleanupPolicy
metadata:
  name: npm-prereleases
spec:
  criteria:
    prerelease_only: true
    older_than_days: 30
    keep_newest: 5
  repositories: [npm-hosted]
  schedule: "0 3 * * *"
```

`metadata.labels` are free-form labels (`[prefix/]name: value`); keys under
`hangar/` are reserved for apply.

**Editor validation.** [`api/hangar-config.schema.json`](../../api/hangar-config.schema.json)
is a JSON schema for these documents, generated from `api/openapi.yaml`. With
the YAML extension of VS Code (or any editor using yaml-language-server), the
first-line comment above enables completion and validation, or map it in the
settings: `"yaml.schemas": {"<path>/api/hangar-config.schema.json": "hangar/**/*.yaml"}`.

## Secrets

Write-only values (upstream passwords and tokens, S3 secret keys, proxy
passwords) are never written into files. Any value can instead be a reference
that apply reads when it runs:

```yaml
token: {valueFrom: {env: NPM_TOKEN}}           # an environment variable
password: {valueFrom: {file: secrets/corp-pw}} # a file, relative to this YAML file
```

A missing or empty variable or file stops apply before anything is sent. One
trailing newline of a file is dropped. Plans and messages print `(secret)`
instead of these values, and passwords in URLs as `REDACTED`. A secret written
literally into a file works but draws a note in the plan.

The server never returns secrets, so apply cannot see whether a stored secret
still equals the declared one: a resource whose only change is a rotated
secret shows as unchanged. Run `hangar apply --update-secrets` after rotating
a secret to send every declared secret again.

## What apply does

`hangar apply` reads every document (`-f` takes files, directories, or `-` for
stdin, repeatable; `-R` also reads subdirectories), reads the current state
from the server, prints the plan, and carries it out:

```
Hangar https://hangar.example.com, apply set "platform"

+ BlobStore/s3-main create
~ Repository/npm-proxy update
    config.proxy.metadata_ttl: "1h0m0s" -> "30m"
~ Repository/npm-hosted update
    labels: adopted into apply set "platform"
+ CleanupPolicy/npm-prereleases create
    note: would delete 12 versions (48.3 MiB) if it ran now
- Repository/old-raw delete
    note: no longer declared
    note: deleting a repository deletes its content

Plan: 2 to create, 2 to update, 1 to delete, 6 unchanged.
```

- **Create** what does not exist, **update** what differs, leave the rest
  alone. Running apply twice in a row changes nothing the second time.
- **Fields you leave out are not managed.** An update sends the server's
  current settings with the declared fields laid over them, and the diff only
  compares declared fields. Removing a field from a file therefore keeps its
  current value; to reset it, declare it (`description: ""`, `auth: null` to
  remove upstream credentials). Lists (group members, privileges) are replaced
  as a whole; objects are merged key by key, except `config.proxy.auth`, which
  is replaced as a whole.
- The comparison knows the API's spelling: `24h` equals `24h0m0s`, `0`,
  `false`, `""`, `[]` and `{}` equal a field the API leaves out, and lists the
  API sorts (role privileges, cleanup policy repositories) compare as sets.
- **Order.** Outbound proxies, then blob stores, roles, repositories, groups
  (a group after the groups it contains) and cleanup policies; deletions in
  the reverse order, after every create and update.
- **Errors.** Each failure names the resource, the server's status, code and
  message, and the fields the server named. A failure skips the later stages
  (a repository is not created on a blob store that failed) and every
  deletion; the other resources of the same stage still apply. After a
  successful create or update apply compares the answer with the file and
  warns about fields the server did not keep (usually a misspelled key).
- `--dry-run` prints the plan and changes nothing; `-v` also lists unchanged
  resources.

Built-in roles and resources defined in the server configuration file (the
`default` blob store, an `http.proxy` outbound proxy) cannot be changed by
apply; the configured blob store accepts only a new `soft_quota_bytes`.

## Ownership and `--prune`

Apply marks everything it creates or adopts with two labels, kept by the
server (`GET /api/v1/resource-labels?kind=...`):

```
hangar/managed-by: hangar-apply
hangar/apply-set:  <name of --apply-set>
```

`--apply-set NAME` names the configuration (default `default`). Use one name
per Git repository or directory that manages a server, e.g. `platform`,
`team-web`.

- **Adoption.** A declared resource that already exists without these labels
  (made in the UI, or before apply was introduced) is taken over: the plan says
  `labels: adopted into apply set "..."`.
- **Conflicts.** A declared resource that another apply set owns is refused,
  so two configurations never fight over one resource. Remove it from one of
  them, or pass `--take-ownership` to move it.
- **Prune.** `hangar apply --prune --apply-set NAME` also deletes resources
  that carry the labels of *that* apply set and are no longer declared.
  Nothing else is ever deleted: not resources made by hand, not those of other
  apply sets, not built-in roles or configured stores. `--prune` refuses to run
  without an explicit `--apply-set`, and apply refuses to run when the files
  hold no resources at all (a wrong path never prunes everything).
- **Labels belong to the resource.** The server binds labels to the identity
  of the resource they were set on. If a pruned (or otherwise deleted)
  resource is created again under the same name by someone else, it starts
  without labels and is not pruned.
- If the token may not read a kind (for example roles), the plan warns and
  does not prune that kind.

The token needs the privileges of what it manages: `system:repos:admin` (or
`repo:<name>:admin` per repository), `system:blobstores:admin`,
`system:cleanup:write`, `system:users:write` for roles and
`system:settings:admin` for outbound proxies; a service account with a role
holding exactly these is the usual choice.

## diff and exit status

`hangar diff -f ...` (or `hangar apply --dry-run`) prints the plan and exits
like `terraform plan -detailed-exitcode`:

| Exit | Meaning |
|---|---|
| 0 | the server matches the files |
| 2 | applying would change something |
| 1 | error (invalid file, missing secret, refused ownership, server error) |

`hangar apply` exits 0 when every change applied, 1 otherwise.

## GitOps in CI

Plan on pull requests, apply on the default branch. The server image
(`ghcr.io/ismoilovdevml/hangar`) is distroless with `hangar` as its
entrypoint, so run it as a command rather than as a job image (or put the
`hangar` binary of a release on the runner). GitHub Actions:

```yaml
name: hangar
on:
  pull_request: {paths: ["hangar/**"]}
  push: {branches: [main], paths: ["hangar/**"]}
concurrency: hangar            # one apply at a time
env:
  HANGAR_URL: https://hangar.example.com
  HANGAR_TOKEN: ${{ secrets.HANGAR_TOKEN }}      # a service-account token
  NPM_TOKEN: ${{ secrets.NPM_TOKEN }}            # referenced by valueFrom
  HANGAR_S3_SECRET: ${{ secrets.HANGAR_S3_SECRET }}
jobs:
  hangar:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: plan or apply
        run: |
          cmd=diff
          [ "${{ github.event_name }}" = push ] && cmd=apply
          docker run --rm -v "$PWD/hangar:/hangar:ro" \
            -e HANGAR_URL -e HANGAR_TOKEN -e NPM_TOKEN -e HANGAR_S3_SECRET \
            ghcr.io/ismoilovdevml/hangar:0.2.1 \
            $cmd -f /hangar -R --apply-set platform --prune || test $? -eq 2 -a $cmd = diff
```

In GitLab CI the same `docker run` works in a job with Docker access. Pass
every variable a `valueFrom: {env: ...}` names into the container (`-e`), and
mount the directory so `valueFrom: {file: ...}` paths resolve.

Drift detection is a scheduled `hangar diff`: exit status 2 means someone
changed the server outside Git.

## export, edit, apply

`hangar export` writes the server's configuration as documents, ready for
apply. Secrets are never exported: each one becomes a `valueFrom: {env: ...}`
reference, and the header lists the variables to set.

```sh
hangar export > hangar/all.yaml                      # everything
hangar export --kind Repository,CleanupPolicy -o repos.yaml
hangar export --apply-set platform                   # what one apply set owns
```

```yaml
# Exported by hangar export. Apply with: hangar apply -f <this file>
# Secrets are never exported. Set these environment variables (or edit the
# references to valueFrom: {file: PATH}) before applying:
#   HANGAR_SECRET_REPOSITORY_NPM_PROXY_AUTH_TOKEN  (Repository/npm-proxy spec.config.proxy.auth.token)
```

Export writes every input field the server returns, defaults included, so a
fresh export applied back shows no changes. Built-in roles and resources from
the server configuration file are left out (listed as notes on stderr). A
NuGet `service_index` other than a plain `v3/index.json` may carry a feed key
and is exported as a reference too.

A typical start: export the server, commit the file, run
`hangar apply --apply-set platform -f ...` once (it adopts everything and
changes nothing else), then edit and apply through merge requests.

## Flags

| Flag | Commands | Meaning |
|---|---|---|
| `--server URL` | all | server base URL; default `$HANGAR_URL` |
| `--token-file FILE` | all | token file; default `$HANGAR_TOKEN`. The token is never printed. |
| `--timeout 30s` | all | limit for each API request attempt |
| `-f PATH` | apply, diff | file, directory or `-`; repeatable |
| `-R` | apply, diff | read subdirectories |
| `--apply-set NAME` | apply, diff, export | owning configuration (export: filter) |
| `--prune` | apply, diff | delete what the apply set owns and the files no longer declare |
| `--take-ownership` | apply, diff | adopt resources owned by another apply set |
| `--update-secrets` | apply, diff | send declared secrets again |
| `--dry-run` | apply | print the plan only |
| `-v` | apply, diff | list unchanged resources too |
| `--kind LIST` | export | kinds to export |
| `-o FILE` | export | output file (written only when the export succeeded) |

Reads are retried up to three times on 429, 502, 503 and 504 (honouring
`Retry-After`, at most 30 s); writes only on 429, which the server answers
before doing anything. Redirects are refused, so the token never follows one
to another host. A warning is printed when the token would travel over plain
`http://` to a host other than localhost.
