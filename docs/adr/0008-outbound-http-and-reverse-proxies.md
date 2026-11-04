# 0008. Outbound HTTP proxies and running behind reverse proxies

Date: 2026-11-03 · Status: Accepted

## Context
Repository managers live inside corporate networks: outbound traffic to Maven
Central, npmjs or Docker Hub must go through an HTTP or SOCKS proxy, often with
a TLS-inspecting proxy CA; some remotes need their own proxy, credentials or
private CA. Inbound, Hangar sits behind load balancers and ingress controllers
that terminate TLS, and Docker clients require a registry at a host root.

## Decision
Outbound (proxy repositories):
- One `core.HTTPClientFactory`, implemented in `internal/httpclient`, builds every
  client used for remotes, so all formats behave identically.
- Server-wide settings in config section `http`: `proxy` (http, https, socks5 URL),
  `proxy_username`/`proxy_password` (password from the environment), `no_proxy`
  (hosts, domain suffixes, CIDRs), `use_environment` (honour
  `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`, default on), `ca_file`, connect and
  response-header timeouts, idle connection limits, `user_agent`.
- Per remote (`ProxyConfig.HTTP`): `proxy` = `inherit` (default) | `none` | URL,
  `ca_bundle` (PEM), `insecure_skip_verify` (explicit, audited), timeouts.
  Upstream credentials (`basic`, `bearer`, or a named `header`) are sent only to
  the remote's own scheme and host, never to redirect targets (CDNs), and never
  override an `Authorization` header the caller set (OCI token flow).
- No overall client timeout: transfers are bounded by request contexts.
  Per remote, `body_timeout` bounds each wait for body bytes (stall
  detection) and broken downloads resume with Range/If-Range when the remote
  advertises byte ranges and a strong validator; connection pools
  (`max_conns_per_host`, `max_idle_conns_per_host`) and HTTP/2
  (`disable_http2`, health pings) are tunable.
- Related hosts (amended 2026-11-04): a remote's own documents may link to
  other hosts (PyPI files on files.pythonhosted.org, nuget.org search on
  azuresearch-*.nuget.org, npm tarball CDNs). Formats fetch them with
  `UpstreamRequest.URL` through the repository's upstream, so the same SSRF
  guard, outbound proxy, request collapsing, negative cache and metrics apply,
  with a circuit breaker and rate-limit hold per host. A host is accepted only
  when it is the remote's, in `ProxyConfig.RelatedHosts`, or in the request's
  format-supplied list (PyPI `file_hosts`, the hosts a NuGet service index
  names); credentials stay on the remote's origin. Formats never build their
  own outbound clients for remote content.
- Rate limits: 429 (and 503 with `Retry-After`) is retried after the
  remote's `Retry-After` within a per-request budget (10s); otherwise the host
  is held until the wait passes and the client gets 429 with `Retry-After`
  (`core.ErrRateLimited`), or the cached copy. 429s are excluded from the
  circuit breaker.

Inbound (reverse proxies):
- `server.base_url` is the single source for every absolute link Hangar
  generates (npm tarball URLs, OCI `WWW-Authenticate` realm, Maven/PyPI links).
- `server.trusted_proxies` (CIDRs/IPs): only from those peers are
  `X-Forwarded-For` (right-most untrusted hop), `X-Forwarded-Proto` and
  `X-Forwarded-Host` believed; the result is `core.RequestInfo`
  (`RemoteIP`, `Scheme`, `Host`) used for audit, rate limiting and connector
  routing.
- Docker connectors (`RepoConfig.Connector`): a dedicated virtual host
  (`cr.example.com`, matched on the effective host) or a dedicated plain-HTTP
  port per repository.

## Consequences
- Proxy behaviour is tested once (`internal/httpclient` tests cover proxy
  selection, no_proxy, socks5, proxy credentials, CA bundles, credential
  scoping on redirects and timeouts) instead of per format.
- Spoofed forwarding headers from untrusted peers are ignored, so audit IPs and
  rate limits cannot be forged.
