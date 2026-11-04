# Hangar user guides

Practical, copy-paste guides for running Hangar and connecting package clients to it.

| Guide | What it covers |
|---|---|
| [Getting started](getting-started.md) | Run the server, create the first administrator and token, create a repository, upload and download a file |
| [Docker / OCI](docker.md) | `docker login/push/pull`, buildx multi-arch, Helm charts over OCI, cosign |
| [npm](npm.md) | npm, pnpm and yarn: `.npmrc`, `.yarnrc.yml`, publish, dist-tags, deprecate, unpublish |
| [Maven](maven.md) | Maven `settings.xml`, Gradle Kotlin and Groovy DSL, `maven-publish` |
| [PyPI](pypi.md) | pip, uv, poetry and twine |
| [NuGet](nuget.md) | `dotnet nuget`, `nuget.config`, push, restore through a group |
| [Raw](raw.md) | Arbitrary files with curl and wget, directory listings |
| [CI integration](ci.md) | GitLab CI, GitHub Actions, Jenkins and Gitea Actions with tokens |
| [REST API](api.md) | Authentication, common calls, pagination, errors |
| [Access management](access.md) | Members, service accounts, roles, privileges and token scopes |
| [Single sign-on](sso.md) | OIDC (Keycloak example) and LDAP / Active Directory sign-in, group to role mapping, troubleshooting |

Operators also need [docs/security.md](../security.md) (secret key, hardening,
rate limits) and [docs/operations/monitoring.md](../operations/monitoring.md)
(metrics, logs, tracing).

Formats compiled into the server today: `docker`, `maven`, `npm`, `nuget`, `pypi` and `raw`
(`GET /api/v1/formats` lists them). Other formats are not supported yet.

## Concepts in one minute

- **Repository**: a named container of one format and one type. Names match
  `^[a-z0-9][a-z0-9-]{1,62}$`.
- **Types**:
  - `hosted`: clients publish into it.
  - `proxy`: fetches from a remote registry on demand and caches what it fetched.
  - `group`: an ordered list of repositories of the same format behind one URL.
    Reads try the members in order. Docker, npm, NuGet and PyPI groups can forward
    writes to one hosted member (`write_member`). Maven and raw groups are read-only.
- **URLs**: every format except Docker is served at
  `<base_url>/repository/<repo>/...`. Docker/OCI is served at `<base_url>/v2/`,
  and the first path segment of the image name selects the repository; a Docker
  repository can also get its own host name or port (a connector, see
  [docker.md](docker.md#connectors-own-host-or-port)).
- **Credentials**: every client authenticates with HTTP Basic (any username plus a
  personal access token as the password) or `Authorization: Bearer <token>`.
  Tokens start with `hgr_`.
- **`server.base_url`** (`HANGAR_SERVER_BASE_URL`) must be the URL clients use.
  Every absolute link Hangar generates is built from it: npm tarball URLs, PyPI
  file links, the Docker token realm, NuGet resource URLs.

## Things that apply to every proxy repository

- Proxy repositories cannot reach loopback, private, link-local or cloud metadata
  addresses by default. To proxy an internal server, list it in
  `http.allow_private_networks` (`HANGAR_HTTP_ALLOW_PRIVATE_NETWORKS`, comma-separated
  CIDRs, IPs, host names or `.domain` suffixes).
- A corporate outbound proxy is configured once in `http.proxy` (`HANGAR_HTTP_PROXY`)
  and inherited by every proxy repository; see `hangar.example.yaml`.
- Common proxy settings in `config.proxy`:

  | Key | Meaning |
  |---|---|
  | `remote_url` | upstream base URL (required) |
  | `metadata_ttl`, `content_ttl`, `negative_cache_ttl` | cache lifetimes, Go durations such as `30m`, `24h` |
  | `auth` | upstream credentials: `{"kind": "basic", "username": "...", "password": "..."}`, `{"kind": "bearer", "token": "..."}` or `{"kind": "header", "header_name": "...", "token": "..."}` |
  | `related_hosts` | other hosts the remote links to (file CDNs, API hosts) that may be fetched on its behalf: host names, `host:port` or `.domain` suffixes; the remote's credentials are never sent there |
  | `offline` | `true`: never contact the remote, serve only cached content (stale included), answer 404 for the rest |
  | `max_concurrent_requests` | cap on requests in flight to the remote (`0` = unlimited) |
  | `http` | per-remote outbound settings: `proxy`, `ca_bundle`, timeouts, connection limits |

- Upstream credentials (`auth`, or a password inside `remote_url`) are encrypted
  with the server's secret key and never returned by the API. Set
  `HANGAR_SECRET_KEY` (`openssl rand -base64 32`, same value on every node) before
  adding them: without it, saving a repository that carries a credential answers
  `501`. See [docs/security.md](../security.md).
- When a remote answers `429`, Hangar passes it on (with `Retry-After`) for content
  it has not cached and keeps serving what it has.
- `POST /api/v1/repositories/<name>/cache:invalidate` makes a proxy forget
  remembered 404s and rate limits so the next request asks the remote again;
  cached content is kept.
- Group members are merged. If a proxy member serves a public package with the
  same name as one of your internal packages, clients may resolve the public one
  (for example a higher version). Use scoped or prefixed names for internal
  packages, or keep the public proxy out of groups that serve internal names.

## Plain HTTP

Hangar's listener speaks plain HTTP; terminate TLS in a reverse proxy or load
balancer and set `server.base_url` to the `https://` URL. List the proxy in
`server.trusted_proxies` so `X-Forwarded-*` headers are honoured (HSTS is then
sent on HTTPS requests). Every guide notes the extra client flag needed when you
test against an `http://` URL.

See also: [docs/FORMATS.md](../FORMATS.md) (protocol details per format) and
[api/openapi.yaml](../../api/openapi.yaml) (full REST reference).
