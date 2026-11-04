# REST API

The management API lives under `<base_url>/api/v1` and speaks JSON. The full
reference is [api/openapi.yaml](../../api/openapi.yaml); this page covers
authentication, the calls you need most, pagination and errors.

Placeholders:

- `https://hangar.example.com`: the server's `server.base_url`.
- `$HANGAR_TOKEN`: a personal access token (`hgr_...`) in an environment variable.
- `<id>`: an id returned by a previous call.

## Authentication

Every request may carry one of:

| Method | Header | Use for |
|---|---|---|
| Bearer token | `Authorization: Bearer hgr_...` | scripts and automation (recommended) |
| Basic, token as password | `Authorization: Basic base64(<any>:hgr_...)` (`curl -u any:$HANGAR_TOKEN`) | tools that only speak Basic |
| Basic, username and password | `curl -u admin:<password>` | bootstrapping the first token; counts towards account lockout |
| Session cookie | `hangar_session` cookie + `X-CSRF-Token` header on writes | the web UI |

Requests without credentials run as the anonymous user and only see repositories
with `anonymous_read`. Wrong, expired or revoked credentials answer `401`; a locked
account answers `423`. Token-authenticated requests get the intersection of the
owner's roles and the token's scopes.

```bash
curl -H "Authorization: Bearer $HANGAR_TOKEN" https://hangar.example.com/api/v1/auth/session
```

returns the caller and its effective privileges:

```json
{"user":{"id":"...","username":"admin","display_name":"admin","email":""},
 "organization":{"slug":"hangar","name":"hangar","environment":"","region":""},
 "privileges":["repo:*:admin","system:repos:admin"]}
```

## Common operations

The examples set the header once:

```bash
H="Authorization: Bearer $HANGAR_TOKEN"
API=https://hangar.example.com/api/v1
```

### Server

```bash
curl "$API/system/status"        # state, version, dependency health
curl "$API/formats"              # formats compiled into this server
```

`/healthz`, `/readyz` and `/metrics` (Prometheus) live at the server root, outside
`/api/v1`.

### Repositories

```bash
# list (filters: type=hosted|proxy|group, format=npm, q=<substring of the name>)
curl -H "$H" "$API/repositories?format=npm&limit=50"

# get one (JSON, or YAML with Accept: application/yaml)
curl -H "$H" "$API/repositories/npm-hosted"
curl -H "$H" -H 'Accept: application/yaml' "$API/repositories/npm-hosted"

# create
curl -H "$H" -H 'Content-Type: application/json' -X POST "$API/repositories" -d '{
  "name": "raw-hosted", "format": "raw", "type": "hosted", "blob_store": "default",
  "config": {"hosted": {"write_policy": "allow"}}
}'

# update (send the whole document; name, format and type cannot change)
curl -H "$H" -H 'Content-Type: application/json' -X PUT "$API/repositories/raw-hosted" -d '{
  "blob_store": "default", "online": true,
  "config": {"hosted": {"write_policy": "allow_once"}, "anonymous_read": true}
}'

# delete
curl -H "$H" -X DELETE "$API/repositories/raw-hosted"
```

Creating, updating and deleting needs `system:repos:admin` or
`repo:<name>:admin`. The JSON shape of each format and type is shown in the client
guides. The YAML returned by `Accept: application/yaml` can be sent back with
`Content-Type: application/yaml`, which suits configuration kept in git.
Upstream secrets are never returned; sending the redacted document back keeps
them.

### Components

A component is one package version (an npm version, a Maven GAV, a Docker tag, a
raw file); its files are assets.

```bash
# list (filters: repository=<exact name>, format=<format>, name=<exact name>;
# sort=-published_at (default) | published_at | name | -downloads)
curl -H "$H" "$API/components?repository=pypi-hosted&limit=20"

curl -H "$H" "$API/components/<id>"            # details, checksums, install command
curl -H "$H" "$API/components/<id>/assets"     # files, with download_url
curl -H "$H" "$API/components/<id>/versions"   # other versions of the same package

# delete a component and its files (needs repo:<repo>:delete)
curl -H "$H" -X DELETE "$API/components/<id>"
```

### Search

```bash
curl -H "$H" "$API/search?q=ledger+format:maven&limit=20"
```

Free text matches name prefixes (`led cor` finds `com.acme:ledger-core`).
Qualifiers: `format:`, `repo:`, `sha256:<hex prefix>`, `license:`,
`security:pending|clean|findings|quarantined`, `unused:<N>d|h|w`,
`published:<date`, `published:>date`, `cve:`. `sort` is `relevance` (default),
`-downloads` or `-published_at`. The response adds `total` and `facets`
(`format`, `security`, `repository`) to the usual page:

```json
{"items":[{"component_id":"...","format":"pypi","coordinates":"acme-tiny==0.2.0",
  "namespace":"","name":"acme-tiny","version":"0.2.0","repository":"pypi-hosted",
  "versions":2,"downloads":0,"security":"pending","published_at":"..."}],
 "next_cursor":null,"total":1,"facets":{"format":[...],"security":[...],"repository":[...]}}
```

### Tokens

```bash
# create a token for yourself (expires_at: RFC 3339, at most 366 days ahead)
curl -H "$H" -H 'Content-Type: application/json' -X POST "$API/tokens" -d '{
  "name": "laptop", "scopes": ["repo:*:read"], "expires_at": "2027-06-30T00:00:00Z"
}'

# create a token for a service account (needs system:tokens:write)
curl -H "$H" -H 'Content-Type: application/json' -X POST "$API/tokens" -d '{
  "name": "gitlab-ci", "owner": "ci-publisher",
  "scopes": ["repo:npm-hosted:read", "repo:npm-hosted:write"], "expires_at": "2027-02-01T00:00:00Z"
}'

# list your tokens (owner=<name> for someone else's, needs system:tokens:read)
curl -H "$H" "$API/tokens"
curl -H "$H" "$API/tokens?owner=ci-publisher"

# revoke (204); tokens minted from it stop working too
curl -H "$H" -X DELETE "$API/tokens/<id>"
```

The secret (`secret` in the create response) is shown once. Listings show only the
`prefix` (`hgr_1Hzo`). A token created by a scoped token cannot exceed the caller's
scopes.

Scopes have the form `repo:<pattern>:<action>` or `system:<area>:<action>`, where
`<pattern>` is a glob (`*`, `npm-*`) and `<action>` is `read`, `write`, `delete`,
`admin` (all of them) or `*`. `system:*` is every system area. Details: [access.md](access.md).

### Users and roles

`/users` and `/roles` manage local users, service accounts and role definitions
(needs `system:users:read` / `system:users:write`). See [access.md](access.md).

```bash
curl -H "$H" -H 'Content-Type: application/json' -X POST "$API/users" -d '{
  "username": "ci-publisher", "kind": "service_account", "roles": ["CI deployer"]
}'
```

### Audit log

```bash
curl -H "$H" "$API/audit?limit=100"                 # needs system:audit:read
curl -H "$H" "$API/audit/export?format=csv" -o audit.csv      # or format=json (NDJSON)
```

See `openapi.yaml` for the filters.

## Pagination

List endpoints share one scheme:

- `limit`: 1 to 1000, default 50. Out of range is `400 invalid_argument`.
- `cursor`: opaque; pass the previous response's `next_cursor` unchanged.
- Response: `{"items": [...], "next_cursor": "..."}`; `next_cursor` is `null` on the
  last page.

```bash
cursor=""
while :; do
  page=$(curl -fsS -H "$H" "$API/repositories?limit=100${cursor:+&cursor=$cursor}")
  echo "$page" | jq -r '.items[].name'
  cursor=$(echo "$page" | jq -r '.next_cursor // empty')
  [ -z "$cursor" ] && break
done
```

`GET /repositories` also returns `counts` (`all`, `hosted`, `proxy`, `group`) for
the current filters.

## Errors

Every error has the same JSON body:

```json
{"code": "invalid_argument", "message": "repository name \"x\" must match ^[a-z0-9][a-z0-9-]{1,62}$: invalid argument"}
```

Some errors add `retry_after` (seconds) or `details`.

| Status | `code` | Meaning |
|---|---|---|
| 400 | `invalid_argument`, `digest_mismatch` | bad input |
| 401 | `unauthenticated` | missing or wrong credentials |
| 403 | `forbidden`, `quarantined`, `csrf_failed` | not allowed; component quarantined; cookie request without a valid `X-CSRF-Token` |
| 404 | `not_found` | does not exist, or you may not read it |
| 405 | `method_not_allowed` | |
| 409 | `already_exists` (or `conflict` on `/users`, `/roles`, `/tokens`), `write_denied` | name taken; write policy refused |
| 423 | `locked` | account locked after failed sign-ins (`Retry-After` set) |
| 429 | `rate_limited` | too many sign-in attempts (`Retry-After` set) |
| 501 | `not_implemented` | endpoint described in `openapi.yaml` but not available yet |
| 502 | `upstream_unavailable` | a proxy remote failed |
| 503 | `offline` | repository offline |
| 500 | `internal` | server error (details only in the server log) |

## Rate limits

Only sign-in is rate limited: `POST /api/v1/auth/login` allows 10 attempts per
minute per client IP (`auth.login_rate`), and five failed password checks within
15 minutes lock an account for 15 minutes (`auth.lockout`). Other API and package
requests are not rate limited yet; put limits in your reverse proxy if you need
them.

## Not implemented yet

`openapi.yaml` also describes endpoints that currently answer
`501 not_implemented`, among them cleanup policies, scheduled tasks, security
findings and waivers, SBOMs, component dependents, identity providers, cluster
nodes and replication. Check the status code before relying on one of them.
