# Corporate HTTP proxy

Proxy repositories fetch from public registries: Maven Central
(`repo1.maven.org`), npmjs (`registry.npmjs.org`), Docker Hub
(`registry-1.docker.io`, `auth.docker.io`, `production.cloudflare.docker.com`),
PyPI (`pypi.org`, `files.pythonhosted.org`), nuget.org (`api.nuget.org` and
its search hosts). In networks where outbound traffic must pass a proxy,
define one or more named proxies, make one the platform default, and let
each proxy repository inherit the default, pick another proxy or connect
directly.

Nothing else in Hangar makes outbound requests (PostgreSQL, S3 and NATS
connections do not use the HTTP proxy settings).

## Named proxies (Settings → Proxies)

Administrators (`system:settings:admin`) manage outbound proxies in
**Settings → Proxies** or through the API, without a restart. The table
lists each proxy's name, host, port, whether it is the platform default and
how many repositories use it.

| Field | Meaning |
|---|---|
| Name | Key repositories refer to (`corp-proxy`); fixed after creation. |
| Scheme, host, port | How Hangar reaches the proxy: `http` or `https`. |
| Username, password | Proxy Basic credentials. The password is sealed with `HANGAR_SECRET_KEY` and write-only. |
| No proxy for | Hosts, `.domain` suffixes, IPs and CIDRs reached directly. |
| Redirect target hosts | Hosts that upstream redirects may lead to and still go through this proxy, such as a registry's CDN (`.cloudflare.docker.com`). When the list is not empty, only the repository's remote host and these hosts use the proxy and everything else is reached directly. When it is empty, every host uses the proxy. |
| CA certificates | PEM trusted for requests through this proxy (the root of a TLS-inspecting proxy). |
| Platform default | Used by every repository that inherits. At most one proxy is the default. |

NTLM proxy authentication is not supported; use Basic credentials or a
proxy that accepts the server's address.

**Test** fetches a URL (default `https://api.nuget.org/v3/index.json`)
through the proxy and shows the HTTP status and the time it took; it works
on unsaved form values too. A `407` means the proxy wants other credentials.

A proxy that repositories select by name cannot be deleted: the API answers
`409 in_use` with the repository names. Point those repositories elsewhere
first. Deleting the default is allowed; inheriting repositories then use
`HTTPS_PROXY` from the environment (with `use_environment`) or connect
directly.

Changes apply on every node within seconds (the event bus, and a 30-second
poll as a safety net). Every change is audited (`proxy.create`,
`proxy.update`, `proxy.delete`, `proxy.test`) without the password.

API (`api/openapi.yaml`):

| Method and path | Purpose |
|---|---|
| `GET /api/v1/proxies` | List (names only for users without `system:settings:read`) |
| `POST /api/v1/proxies` | Create |
| `GET`, `PUT`, `DELETE /api/v1/proxies/{name}` | Read, replace, delete |
| `POST /api/v1/proxies/{name}:test` | Fetch `url` through it (`{"url": "...", "settings": {...}}`) |

```sh
curl -u admin -X POST https://hangar.example.com/api/v1/proxies \
  -H 'Content-Type: application/json' -d '{
    "name": "corp-proxy", "scheme": "http", "host": "proxy.example.com", "port": 3128,
    "username": "hangar", "password": "…", "no_proxy": [".corp.example", "10.0.0.0/8"],
    "redirect_hosts": [], "ca_pem": "", "default": true}'
```

### Precedence

1. A proxy set in the configuration file (`http.proxy` below) is listed as
   `config`, read-only and labelled *set in configuration file*. It is the
   platform default; no proxy from the UI can take that role.
2. Otherwise the proxy marked *Platform default* in the UI.
3. Otherwise, with `http.use_environment` (the default), `HTTPS_PROXY`,
   `HTTP_PROXY` and `NO_PROXY` of the server's environment.
4. Otherwise a direct connection.

`http.ca_file` is trusted for every request in addition to each proxy's own
CA certificates.

## Server-wide settings

```yaml
http:
  proxy: http://proxy.corp.example:3128   # http://, https:// or socks5://
  proxy_username: hangar
  # proxy_password: from HANGAR_HTTP_PROXY_PASSWORD
  no_proxy:
    - .corp.example          # domain suffix
    - legacy-registry.corp.example # host
    - 10.0.0.0/8             # CIDR
  use_environment: true      # with proxy empty: honour HTTP_PROXY/HTTPS_PROXY/NO_PROXY
  ca_file: /etc/hangar/ca/corp-root.pem
  connect_timeout: 10s
  read_timeout: 60s          # wait for response headers
```

| Setting | Env | Meaning |
|---|---|---|
| `http.proxy` | `HANGAR_HTTP_PROXY` | The platform default proxy, listed as `config` in the UI. |
| `http.proxy_username` | `HANGAR_HTTP_PROXY_USERNAME` | Proxy credentials (Basic). |
| `http.proxy_password` | `HANGAR_HTTP_PROXY_PASSWORD` | Keep it in the environment / a Secret. |
| `http.no_proxy` | `HANGAR_HTTP_NO_PROXY` (comma-separated) | Reached directly: hosts, `.domain` suffixes, CIDRs. |
| `http.use_environment` | `HANGAR_HTTP_USE_ENVIRONMENT` | When `http.proxy` is empty, use the standard `HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY` variables. |
| `http.ca_file` | `HANGAR_HTTP_CA_FILE` | PEM bundle trusted in addition to the system roots: the root of a TLS-inspecting proxy, or a private CA of an internal mirror. |
| `http.allow_private_networks` | `HANGAR_HTTP_ALLOW_PRIVATE_NETWORKS` | Internal addresses proxy repositories may reach (see below). |

Where to set them:

- Compose: `HANGAR_HTTP_PROXY`, `HANGAR_HTTP_PROXY_USERNAME`,
  `HANGAR_HTTP_PROXY_PASSWORD`, `HANGAR_HTTP_NO_PROXY` in `.env`. For a CA
  file, mount it into the container (an extra `volumes:` entry in a compose
  override) and set `http.ca_file` in `deploy/compose/hangar.yaml`.
- Helm: `config.http.*`, the password in `httpProxyPassword`, the CA via
  `extraVolumes`/`extraVolumeMounts` ([install-helm.md](install-helm.md#private-ca-and-corporate-proxy)).
  Add the cluster's internal domains (`.svc`, `.cluster.local`) to `no_proxy`.
- systemd: `/etc/hangar/hangar.yaml` and `HANGAR_HTTP_PROXY_PASSWORD` in
  `/etc/hangar/hangar.env`.

## Per repository

In the repository settings, **Outbound proxy** offers *Platform default*
(the default), each named proxy, and *No proxy (direct)*. In the API that is
`config.proxy.http.proxy_name` (a named proxy), or `proxy`. The rest of
`config.proxy.http` tunes the connection for this remote only:

```json
"proxy": {
  "remote_url": "https://repo1.maven.org/maven2/",
  "http": {
    "proxy": "none",
    "ca_bundle": "-----BEGIN CERTIFICATE-----\n...",
    "connect_timeout": "5s",
    "read_timeout": "120s"
  }
}
```

`proxy_name` names a proxy from `GET /api/v1/proxies`; saving a repository
with an unknown name is refused. `proxy` is `inherit` (default: the platform
default proxy, honouring its `no_proxy`), `none` (direct), or a URL,
optionally with `user:password@`, kept for repositories configured before
named proxies existed; the password is stored encrypted
(`HANGAR_SECRET_KEY` required) and never returned by the API. `insecure_skip_verify` exists for emergencies, is audited, and should
not be used.

## Internal mirrors and the address policy

To prevent server-side request forgery, proxy repositories may not connect
to loopback, private (RFC 1918), link-local (cloud metadata), CGNAT and other
internal ranges: such a remote is refused when the repository is saved and on
every connection. When the remote really is internal (an old repository
manager being migrated, an internal mirror), allow it explicitly:

```yaml
http:
  allow_private_networks:
    - 10.20.30.0/24
    - .corp.example
```

The policy applies to the address Hangar dials. Through a proxy, that is the
proxy's address, so an internal proxy needs to be listed as well when it
resolves to a private address.

## Checking it

- The repository's status in the UI and `GET /api/v1/repositories/<name>`
  show an unreachable remote; the logs carry the outbound error (proxy refused
  the connection, TLS verification failed, timeout).
- Metrics: `hangar_upstream_fetches_total` and `hangar_upstream_state` and the upstream dashboard in
  `deploy/observability/grafana/` break failures down by remote.
- A TLS error `x509: certificate signed by unknown authority` on every remote
  means the proxy inspects TLS: set `http.ca_file` to its root certificate.
