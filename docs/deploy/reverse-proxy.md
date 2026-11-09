# TLS and reverse proxies

Hangar serves plain HTTP on `server.addr` (`:8080`). TLS terminates in front of
it. Whatever the proxy, four things must hold:

1. **`server.base_url`** is the public HTTPS URL. Hangar builds every absolute
   link from it (npm tarball URLs, the Docker token realm, Maven and PyPI
   links), never from the request's `Host` header.
2. **`server.trusted_proxies`** lists the proxy's address (IP or CIDR). Only
   from these peers does Hangar believe `X-Forwarded-For`, `-Proto` and
   `-Host`. Without it every client appears as the proxy: audit logs show one
   IP and all clients share one rate-limit bucket (`security.rate_limit`,
   `auth.login_rate`), which quickly answers 429 to everybody. HSTS is sent
   only on requests Hangar knows were HTTPS, that is with
   `X-Forwarded-Proto: https` from a trusted proxy.
3. **Large, slow transfers**: no request body limit, no buffering of request
   or response bodies, and timeouts measured between reads rather than for the
   whole request. Docker layers and Maven or raw artifacts can be many GiB.
4. **`/metrics` stays internal**: by default it has no authentication
   (`metrics.auth: none`). Block it on public hosts and let Prometheus scrape
   the nodes directly; `metrics.auth` and `metrics.allowed_cidrs` protect it
   further ([monitoring.md](../operations/monitoring.md#protecting-metrics)).

## Docker clients and the registry host

Docker requires the registry API at the root of a host (`/v2/`). Hangar
serves it on its main host, so `docker login hangar.example.com` and
`docker push hangar.example.com/<repository>/<image>:<tag>` work without
anything else. The examples below also add a separate **registry host**
(`registry.example.com`) that routes only `/v2/`, which gives image names a
dedicated host name and lets you apply separate limits or certificates to
Docker traffic. Registry tokens are always issued by `server.base_url`
(the realm in the 401 challenge), so the UI host must be reachable by Docker
clients as well.

For one Docker repository per host name without a repository prefix in image
names, use a **host connector** (`"connector": {"host": "cr.example.com"}`,
[guides/docker.md](../guides/docker.md#connectors-own-host-or-port)). Route
that name to Hangar like the registry host and keep the original `Host`
header (or send `X-Forwarded-Host`).

## Caddy

`deploy/compose/Caddyfile` is a complete example used by the compose stack:
automatic certificates for both hosts, HTTP/3, `flush_interval -1` so
downloads and the live event stream are passed through immediately, the
registry host limited to `/v2`, and `/metrics` answered with 404. Caddy has no
request body limit and does not buffer request bodies by default, and its
server timeouts are set to `read_header 30s` and `idle 5m` only, so long
transfers are never cut off.

On a host without Docker, replace `hangar:8080` with `127.0.0.1:8080` and set
`server.trusted_proxies: [127.0.0.1]`. Without Internet access for ACME, add
`tls /path/cert.pem /path/key.pem` (your CA) or `tls internal` to each site.
Check a changed file with `caddy validate --config Caddyfile`.

## Traefik

Traefik v3 aborts request bodies after **60 s** by default
(`entryPoints.<name>.transport.respondingTimeouts.readTimeout`), which breaks
every large upload. Disable the whole-request timeouts on the entry point
Hangar uses. Traefik streams bodies and has no size limit unless you add the
`buffering` middleware; do not add it for Hangar.

Static configuration, `/etc/traefik/traefik.yml`:

```yaml
entryPoints:
  web:
    address: ":80"
    http:
      redirections:
        entryPoint:
          to: websecure
          scheme: https
  websecure:
    address: ":443"
    transport:
      respondingTimeouts:
        readTimeout: 0s      # default 60s aborts large uploads
        writeTimeout: 0s     # large downloads
        idleTimeout: 180s
    http:
      tls:
        certResolver: letsencrypt

certificatesResolvers:
  letsencrypt:
    acme:
      email: ops@example.com
      storage: /var/lib/traefik/acme.json   # the directory must exist and be writable
      httpChallenge:
        entryPoint: web

providers:
  file:
    filename: /etc/traefik/dynamic/hangar.yml
    watch: true
```

Dynamic configuration, `/etc/traefik/dynamic/hangar.yml`:

```yaml
http:
  routers:
    # UI, REST API and every package format.
    hangar:
      rule: Host(`hangar.example.com`)
      entryPoints: [websecure]
      service: hangar
    # Unauthenticated metrics: only from the local Prometheus (longer rule wins).
    hangar-metrics:
      rule: Host(`hangar.example.com`) && Path(`/metrics`)
      entryPoints: [websecure]
      middlewares: [hangar-internal-only]
      service: hangar
    # Docker / OCI clients only.
    hangar-registry:
      rule: Host(`registry.example.com`) && PathPrefix(`/v2`)
      entryPoints: [websecure]
      service: hangar

  middlewares:
    hangar-internal-only:
      ipAllowList:
        sourceRange: ["127.0.0.1/32"]

  services:
    hangar:
      loadBalancer:
        # Traefik passes Host and X-Forwarded-* by default.
        servers:
          - url: http://127.0.0.1:8080
          # - url: http://10.0.0.12:8080   # more Hangar nodes
        healthCheck:
          path: /readyz
          interval: 10s
          timeout: 3s
```

Hangar: `server.trusted_proxies: [127.0.0.1]` (Traefik on the same host) or
Traefik's address. Both files were loaded by Traefik v3.7 for this guide:
all three routers come up enabled and the `websecure` entry point reports no
read or write timeout.

## Kubernetes ingress

The Helm chart writes the Ingress for you ([install-helm.md](install-helm.md)):

- `ingress.preset: nginx` (ingress-nginx) adds `proxy-body-size: "0"`,
  `proxy-request-buffering: "off"`, `proxy-buffering: "off"`,
  `proxy-http-version: "1.1"` and 3600 s read and send timeouts.
- `ingress.preset: traefik` marks the router for TLS. Set the entry point
  timeouts in Traefik itself, for example in the Traefik Helm chart:

  ```yaml
  additionalArguments:
    - --entryPoints.websecure.transport.respondingTimeouts.readTimeout=0s
    - --entryPoints.websecure.transport.respondingTimeouts.writeTimeout=0s
  ```

- Other controllers (`preset: none`): apply the equivalent of the four rules
  above with `ingress.annotations`.
- `ingress.registryHost` adds the registry host (`/v2` only), and
  `ingress.blockMetrics` (default on) sends the metrics path to a Service
  without endpoints, so it answers 503 publicly.
- Gateway API: `httpRoute.*` creates the same routes as HTTPRoutes; set
  per-rule `timeouts` if your implementation enforces a request timeout.
- `config.server.trusted_proxies` defaults to the private address ranges
  (any pod); narrow it to the ingress controller's pod CIDR.

## Cloud load balancers

An L7 load balancer (AWS ALB, GCP HTTPS LB, Azure Application Gateway) in front
of Hangar needs: an idle or backend timeout of several minutes (ALB defaults to
60 s), no request size limit (check Application Gateway's body limits), a
health check on `/readyz`, and its address range in
`server.trusted_proxies`. An L4 (TCP) load balancer in front of a proxy needs
nothing special; with PROXY protocol, enable it on both sides.

## Checking it

```bash
# The proxy passes the scheme: Strict-Transport-Security appears on HTTPS.
curl -sI https://hangar.example.com/ | grep -i strict-transport
# /metrics is not public.
curl -s -o /dev/null -w '%{http_code}\n' https://hangar.example.com/metrics   # 404 or 403 or 503
# The registry host only serves /v2/ and answers with the token realm.
curl -sI https://registry.example.com/v2/ | grep -i www-authenticate
# A large upload passes (5 GiB to a raw repository, then delete it).
head -c 5368709120 /dev/urandom > big.bin
curl -fsS -u admin -T big.bin https://hangar.example.com/repository/raw-hosted/big.bin
```
