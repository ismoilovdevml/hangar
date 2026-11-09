# Automate Hangar: the REST API

Everything the web UI does goes through a JSON REST API under
`<base_url>/api/v1`. Scripts, CI jobs, configuration tools and other automated
clients use the same API with an access token. This page is the starting point:
how to connect an integration, what a token may do, the calls you need most,
pagination, errors and rate limits.

Every server also publishes its own reference, with its real URLs:

| Where | What |
|---|---|
| `<base_url>/api-docs` | API reference in the web UI: every endpoint by area, schemas, curl examples and "Try it" (also under Settings → API reference) |
| `<base_url>/api/v1/openapi.yaml`, `/api/v1/openapi.json` | the OpenAPI 3.1 document ([api/openapi.yaml](../../api/openapi.yaml)); public, no token needed |
| `<base_url>/api/v1/automation-guide.md` | a short Markdown guide for automated clients: authentication, errors, paging, limits and examples |
| `<base_url>/llms.txt` | a plain-text index of the documents above, at the conventional root path |

Placeholders:

- `https://hangar.example.com`: the server's `server.base_url`.
- `$HANGAR_TOKEN`: a personal access token (`hgr_...`) in an environment variable.
- `<id>`: an id returned by a previous call.

## Connect an integration

1. **Create a token.** In the web UI: My account → Personal access tokens →
   New token for yourself, or Access & audit → Service accounts → (account) →
   Tokens → New token for a shared integration. Prefer a service account per
   integration: its actions are audited under its own name and it does not
   stop working when a person leaves.
2. **Give it only what it needs.** Pick a preset or build a custom set of
   permissions (see [Tokens](#tokens) below), a short expiry, and optionally
   the networks it may be used from and a read-only flag.
3. **Hand it over.** After creation the secret is shown once, with
   ready-to-paste snippets: a curl call, `HANGAR_URL` / `HANGAR_TOKEN` lines,
   package-client configuration (npm, NuGet, pip, Maven, docker) and a block
   for automation that also points at `/llms.txt` and the OpenAPI document.
4. **Let the client check itself.** `GET /api/v1/me` answers who the caller is,
   what it may do and when its token expires.

```bash
export HANGAR_URL=https://hangar.example.com
export HANGAR_TOKEN=hgr_...
curl -fsS -H "Authorization: Bearer $HANGAR_TOKEN" "$HANGAR_URL/api/v1/me"
```

```json
{"user":{"username":"ci-publisher","kind":"service_account", ...},
 "privileges":["repo:npm-hosted:read","repo:npm-hosted:write"],
 "auth_method":"token",
 "token":{"id":"...","kind":"personal","name":"release pipeline","prefix":"hgr_1Hzo",
          "scopes":["repo:npm-hosted:read","repo:npm-hosted:write"],
          "expires_at":"2027-02-01T00:00:00Z","read_only":false,"allowed_cidrs":["10.20.0.0/16"]},
 "token_policy":{"max_lifetime":"8784h0m0s","max_lifetime_days":366,"expiry_warning":"336h0m0s"}, ...}
```

`privileges` is what this request may do: the owner's roles narrowed by the
token's scopes. `GET /auth/session` returns the same body.

## Authentication

Every request may carry one of:

| Method | Header | Use for |
|---|---|---|
| Bearer token | `Authorization: Bearer hgr_...` | scripts and automation (recommended) |
| Basic, token as password | `Authorization: Basic base64(<any>:hgr_...)` (`curl -u any:$HANGAR_TOKEN`) | tools that only speak Basic |
| Basic, username and password | `curl -u admin:<password>` | bootstrapping the first token; counts towards account lockout |
| Session cookie | `hangar_session` cookie + `X-CSRF-Token` header (the `hangar_csrf` cookie's value) on writes | the web UI; [first sign-in](#first-sign-in-from-a-script) |

Every route requires sign-in except `GET /system/status`, `GET /system/version`,
`GET /auth/providers`, `POST /auth/login` and the documents `GET /openapi.yaml`,
`GET /openapi.json` and `GET /automation-guide.md`; anonymous calls to anything else
answer `401`. (Package endpoints are different: repositories with
`anonymous_read` serve downloads without credentials.) Wrong, expired or revoked
credentials answer `401` (with `WWW-Authenticate: Bearer realm="Hangar API"`) on
every route, the public ones included, and so do package endpoints, a public
service index or signing key included: a client with a bad token learns it on its
first request. Leave the `Authorization` header out to call public routes
anonymously. Failed attempts count towards the per-IP limit
(`security.rate_limit.failed_auth`); a locked account answers `423`. Token-authenticated
requests get the intersection of the owner's roles and the token's scopes.

Browsers may call the API only from `server.base_url`'s origin; list other origins
in `security.cors.allowed_origins` if a separate web app needs access.

```bash
curl -H "Authorization: Bearer $HANGAR_TOKEN" https://hangar.example.com/api/v1/auth/session
```

returns the caller and its effective privileges (see
[Connect an integration](#connect-an-integration) for the token section).

### Session cookies and CSRF

`POST /auth/login` signs in with a username and password and answers with two
cookies: `hangar_session` (the session, HttpOnly) and `hangar_csrf` (readable).
The same CSRF value is also in the response's `X-CSRF-Token` header. Every
request authenticated by the session cookie that is not GET or HEAD must echo
it in an `X-CSRF-Token` header, or it answers `403 csrf_failed`. Requests with
a token or Basic credentials need no CSRF header. Changing the password starts
a new session with a new CSRF value: read the cookie again afterwards.

### First sign-in from a script

A new administrator (from `bootstrap.admin_password` or `hangar admin
reset-password`) must set their own password before anything else; every other
call answers `403 password_change_required`. To get from there to a token
without the web UI, with a session:

```bash
jar=$(mktemp)
csrf() { awk '$6 == "hangar_csrf" { print $7 }' "$jar"; }

# 1. Sign in; the cookies land in the jar.
curl -fsS -c "$jar" -H 'Content-Type: application/json' -X POST "$HANGAR_URL/api/v1/auth/login" \
  -d "$(jq -n --arg p "$ONE_TIME_PASSWORD" '{username: "admin", password: $p}')"

# 2. Set the new password: a session write, so it carries X-CSRF-Token.
curl -fsS -b "$jar" -c "$jar" -H "X-CSRF-Token: $(csrf)" -H 'Content-Type: application/json' \
  -X PUT "$HANGAR_URL/api/v1/me/password" \
  -d "$(jq -n --arg c "$ONE_TIME_PASSWORD" --arg n "$NEW_PASSWORD" '{current_password: $c, new_password: $n}')"

# 3. Create a token with the new session (and its new CSRF value).
curl -fsS -b "$jar" -H "X-CSRF-Token: $(csrf)" -H 'Content-Type: application/json' \
  -X POST "$HANGAR_URL/api/v1/tokens" \
  -d '{"name": "bootstrap", "scopes": ["system:*"], "expires_at": "2027-01-01T00:00:00Z"}' | jq -r .secret
rm -f "$jar"
```

Over plain HTTP the cookies are only sent back when `auth.session.insecure` is
set; use HTTPS. Basic credentials skip the cookies and CSRF altogether:
`curl -u "admin:$ONE_TIME_PASSWORD" -X PUT .../me/password` with the same body,
then `curl -u "admin:$NEW_PASSWORD" -X POST .../tokens`.

On the server itself, `hangar admin token create --user admin --name ci
--scopes 'system:*' --expires 90d` prints a token without any sign-in, and
`hangar admin reset-password --user NAME` prints a new one-time password (it
also clears a lockout). Both read the same configuration as `hangar serve`.

## Common operations

The examples set the header once:

```bash
H="Authorization: Bearer $HANGAR_TOKEN"
API=https://hangar.example.com/api/v1
```

### Server

```bash
curl "$API/system/status"           # no sign-in: state, version, base_url, registry_host, dependency health
curl -H "$H" "$API/formats"         # formats compiled into this server
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

# proxy only: forget remembered 404s and upstream rate limits (cached content stays)
curl -H "$H" -X POST "$API/repositories/npm-proxy/cache:invalidate"
# -> {"repository":"npm-proxy","negative_entries":3,"rate_limited_hosts":0}
```

Creating, updating, deleting and cache invalidation need `system:repos:admin` or
`repo:<name>:admin`. The JSON shape of each format and type is shown in the client
guides. The YAML returned by `Accept: application/yaml` can be sent back with
`Content-Type: application/yaml`, which suits configuration kept in git.
Upstream secrets are never returned; sending the redacted document back keeps
them. Saving a repository with upstream credentials needs `HANGAR_SECRET_KEY` on
the server (`501` otherwise).

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

# list your tokens
curl -H "$H" "$API/tokens"
# someone else's, or everyone's (needs system:tokens:read);
# filters: owner, owner_kind=user|service_account, status=active|expired, q=<name substring>
curl -H "$H" "$API/tokens?owner=ci-publisher"
curl -H "$H" "$API/tokens?all=true&owner_kind=service_account&status=active"

# revoke (204); tokens minted from it stop working too
curl -H "$H" -X DELETE "$API/tokens/<id>"

# restrict a token to networks and to reading
curl -H "$H" -H 'Content-Type: application/json' -X POST "$API/tokens" -d '{
  "name": "dashboard", "scopes": ["repo:*:read", "system:audit:read"], "expires_at": "2027-01-31T00:00:00Z",
  "allowed_cidrs": ["10.20.0.0/16", "203.0.113.7"], "read_only": true
}'

# rotate: a new secret with the same name, scopes, restrictions and lifetime;
# the old token keeps working for 24 hours ("0s": revoked now; omit: kept)
curl -H "$H" -H 'Content-Type: application/json' -X POST "$API/tokens/<id>/rotate" -d '{"revoke_old_after": "24h"}'
# -> the new token with "secret", plus "replaced": {"id", "revoked", "expires_at"}
```

The secret (`secret` in the create response) is shown once. Listings show only the
`prefix` (`hgr_1Hzo`) plus `owner_kind`, `last_used_at`, `last_used_ip`,
`allowed_cidrs` and `read_only`. A token created by a scoped token cannot exceed
the caller's scopes, and a read-only token can only create read-only tokens.

A token never does more than its owner: every request needs both a privilege
from the owner's roles and a matching scope, checked again on each request, so
taking a role away from the owner narrows the token at once. Scopes the owner
lacks are accepted but have no effect; the web UI greys them out and says why.

Restrictions, both optional:

- `allowed_cidrs`: up to 32 addresses or CIDR networks. A request from any other
  client address fails authentication with `401` and is audited as
  `auth.token.failed` with reason `address_not_allowed`. Behind a reverse proxy
  the client address comes from `X-Forwarded-For` only when the proxy is in
  `server.trusted_proxies`.
- `read_only`: the token's scopes are narrowed to `read`, and the REST API
  answers `403 read_only_token` to anything other than GET and HEAD.

Registry logins (docker, and other clients that trade the token for a short
session token) inherit both restrictions.

Rotation keeps the old lifetime (capped at the server maximum). Rotating your
own token needs nothing more; a service account's needs `system:tokens:write`,
plus `system:tokens:delete` to end the old one. Nobody can rotate another
person's token. Rotations are audited as `token.rotate`.

Presets in the web UI and the scopes they create:

| Preset | Scopes |
|---|---|
| Read only | `repo:*:read` |
| Publish to selected repositories | `repo:<name or glob>:read` and `:write` for each pick |
| Repository admin | `repo:<name or glob>:admin` for each pick |
| Security reviewer | `repo:*:read`, `system:security:read`, `system:security:write` |
| Full admin | `system:*`, `repo:*:admin` |
| Custom | any repositories × read / write / delete / admin, plus read / write / admin per administration area |

Scopes have the form `repo:<pattern>:<action>` or `system:<area>:<action>`, where
`<pattern>` is a glob (`*`, `npm-*`) and `<action>` is `read`, `write`, `delete`,
`admin` (all of them) or `*`. `system:*` is every system area. Details: [access.md](access.md).

### Users and roles

`/users`, `/roles` and `/me` manage members, service accounts, roles, role
membership, sessions, passwords and effective permissions (needs
`system:users:read` / `system:users:write` / `system:users:delete` for other
people). The full list of operations is in [access.md](access.md).

```bash
curl -H "$H" -H 'Content-Type: application/json' -X POST "$API/users" -d '{
  "username": "ci-publisher", "kind": "service_account", "roles": ["CI deployer"]
}'
curl -H "$H" "$API/users/<id>/permissions"      # what this user may do, and why
```

### Cleanup policies and tasks

Cleanup policies delete components by age, download recency, pre-release status
or a keep-newest count (needs `system:cleanup:*`). New or changed selection rules
must be previewed first; the preview returns a `preview_token` (valid one hour)
to send with the save:

```bash
POLICY='{"name": "old-snapshots", "format": "maven", "repositories": ["maven-snapshots"],
         "criteria": {"older_than_days": 30, "keep_newest": 5}}'

curl -H "$H" -H 'Content-Type: application/json' -X POST "$API/cleanup-policies/preview" -d "$POLICY"
# -> {"component_count":..., "bytes":..., "sample":[...], "preview_token":"cpv_...", ...}

curl -H "$H" -H 'Content-Type: application/json' -X POST "$API/cleanup-policies" -d '{
  "name": "old-snapshots", "format": "maven", "repositories": ["maven-snapshots"],
  "criteria": {"older_than_days": 30, "keep_newest": 5},
  "schedule": "0 3 * * *", "preview_token": "cpv_..."
}'

curl -H "$H" -X POST "$API/cleanup-policies/old-snapshots/run"   # run now
curl -H "$H" "$API/cleanup-policies/old-snapshots/runs"
```

`schedule` is a five-field cron expression in UTC; omit it to run only on demand.
Other criteria: `not_downloaded_days`, `keep_matching` (globs),
`version_include` / `version_exclude` (regular expressions), `prerelease_only`,
`min_size_bytes`. Scheduled jobs (built-in maintenance such as blob garbage
collection and audit partitions, plus one task per scheduled policy) are listed
and controlled under `/tasks`
(`GET /tasks`, `POST /tasks/<id>/run`, `GET /tasks/<id>/runs/<run_id>/log`; needs
`system:tasks:*`).

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
the current filters. `sort` orders the list before paging: `name` (default),
`type`, `format` or `last_activity`, with a leading `-` to reverse. Each
repository carries `last_activity_at`, the latest upload, cache fill or download
(for a group, in its direct members), or `null`.

## Errors

Every error has the same JSON body:

```json
{"code": "invalid_argument", "message": "repository name \"x\" must match ^[a-z0-9][a-z0-9-]{1,62}$: invalid argument"}
```

Some errors add `retry_after` (seconds) or `details`.

| Status | `code` | Meaning |
|---|---|---|
| 400 | `invalid_argument`, `digest_mismatch`, `preview_required` | bad input; a cleanup policy saved without a valid `preview_token` |
| 401 | `unauthenticated` | missing or wrong credentials, or sign-in required |
| 403 | `forbidden`, `read_only_token`, `quarantined`, `csrf_failed`, `cors_denied` | not allowed; a write with a read-only token; component quarantined; cookie request without a valid `X-CSRF-Token`; browser call from an origin not allowed |
| 404 | `not_found` | does not exist, you may not read it, or no such endpoint |
| 405 | `method_not_allowed` | |
| 409 | `already_exists` (or `conflict` on `/users`, `/roles`, `/tokens`), `write_denied`, `already_running`, `not_running`, `managed` | name taken; write policy refused; task or policy run state |
| 413 | `body_too_large` | request body over `security.limits.api_body_bytes` (1 MiB) |
| 423 | `locked` | account locked after failed password checks (`Retry-After` set) |
| 429 | `rate_limited` | a rate limit below was hit (`Retry-After` set) |
| 501 | `not_implemented` | endpoint described in `openapi.yaml` but not available yet |
| 502 | `upstream_unavailable` | a proxy remote failed |
| 503 | `offline` | repository offline |
| 500 | `internal` | server error (details only in the server log) |

## Rate limits

Limits are per client IP (IPv6 per /64). The client IP comes from
`X-Forwarded-For` only when the peer is in `server.trusted_proxies`; otherwise
every client behind a reverse proxy shares the proxy's budget.

| Limit | Default | Setting |
|---|---|---|
| API requests (`/api/v1`) | 1200 per minute, burst 300 | `security.rate_limit.api` |
| Failed authentications (`401`) on the API and package endpoints | 30 per minute, burst 30 | `security.rate_limit.failed_auth` |
| Sign-in attempts (`POST /auth/login`) | 10 per minute, burst 10 | `auth.login_rate` |
| Failed password checks per account | 5 in 15 minutes, then locked 15 minutes (`423`) | `auth.lockout` |

Clients should retry only `429` and `5xx` answers, after the `Retry-After`
seconds (also `retry_after` in the body), with backoff; other `4xx` answers will
not change on a retry.

When an address runs out of its failed-authentication budget, every request from
it that carries credentials answers `429`, even with a valid token, until the
budget refills. A misconfigured CI job with a revoked token can therefore block
the other jobs on the same runner address for a short while. `per_minute: 0`
disables a limit. Details in [docs/security.md](../security.md).

## Not implemented yet

`openapi.yaml` also describes endpoints that currently answer
`501 not_implemented`: component SBOMs and dependents, moving repositories
between blob stores, and replication. Check the status code before relying on one of them. A
path the document does not describe at all answers `404 not_found`.
