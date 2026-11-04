# Corporate HTTP proxy

Proxy repositories fetch from public registries: Maven Central
(`repo1.maven.org`), npmjs (`registry.npmjs.org`), Docker Hub
(`registry-1.docker.io`, `auth.docker.io`, `production.cloudflare.docker.com`),
PyPI (`pypi.org`, `files.pythonhosted.org`), nuget.org (`api.nuget.org` and
its search hosts). In networks where outbound traffic must pass a proxy,
configure it once for the whole server; each proxy repository can override it.

Nothing else in Hangar makes outbound requests (PostgreSQL, S3 and NATS
connections do not use the HTTP proxy settings).

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
| `http.proxy` | `HANGAR_HTTP_PROXY` | Proxy for all outbound requests of proxy repositories. |
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

A proxy repository's `config.proxy.http` overrides the server settings for
its remote only:

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

`proxy` is `inherit` (default: the server settings, honouring `no_proxy`),
`none` (direct), or a URL, optionally with `user:password@`; the password is
stored encrypted (`HANGAR_SECRET_KEY` required) and never returned by the
API. `insecure_skip_verify` exists for emergencies, is audited, and should
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
