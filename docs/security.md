# Security

How Hangar protects the artifacts, credentials and accounts it holds, what
an operator must configure, and how to report a vulnerability
([SECURITY.md](../SECURITY.md)).

## Threat model (summary)

| Asset | Threat | Control |
|---|---|---|
| Private packages | Read or overwrite by a caller without rights | Every API operation and package endpoint authorizes the principal (`repo:<name>:read/write/delete/admin`, `system:*`); token scopes narrow a user's roles. Enforced twice: at the HTTP boundary and in the services that mutate. Checked end to end by `test/security` (below). |
| Accounts | Password guessing, credential stuffing, token guessing | argon2id hashes, per-account lockout, per-IP login rate limit, per-IP limit on failed authentications on the API and package endpoints, tokens with 238 bits of entropy and a checksum, stored as SHA-256. |
| Browser sessions | CSRF, XSS, clickjacking, session theft through uploaded content | `HttpOnly`/`Secure`/`SameSite` session cookie, CSRF token on mutations, origin check, CORS deny-by-default, CSP for the UI, sandboxed CSP for everything served from package endpoints, `X-Frame-Options: DENY`, `frame-ancestors 'none'`, `nosniff`. |
| Single sign-on | Login CSRF, code interception, token substitution, account takeover by name | OIDC authorization code flow with PKCE (S256); state, nonce and verifier in a short-lived AES-GCM encrypted `HttpOnly` cookie (key derived from `security.secret_key`); ID token signature, issuer, audience, expiry and nonce verified; same-origin post-login redirects only. External identities are linked by issuer and subject and never take over a local, service or other external account. LDAP: filter values escaped, exactly one entry required, empty passwords refused (no anonymous bind). |
| Upstream credentials and S3 keys | Disclosure through a database dump, backup, read-only SQL access or the API | Envelope encryption at rest (below); never returned by the API; opened only when the outbound client or the storage driver is built; never sent to redirect targets. |
| Internal network | Server-side request forgery through proxy repositories | Outbound address policy: loopback, private, link-local (cloud metadata), CGNAT, multicast, reserved and IPv4-embedding IPv6 ranges are refused when a proxy is saved, on every redirect hop and when a connection is dialled. Only `http.allow_private_networks` opens exceptions. |
| Availability | Slow-header, slow-body and oversized requests, API floods | `server.read_header_timeout`, body idle timeout, header and API body limits, per-IP API rate limit; uploads are streamed, never buffered. |
| Audit trail | Repudiation | Every mutation, denial and failed sign-in is recorded in the append-only audit log with the client IP (forwarding headers honoured only from `server.trusted_proxies`). |

Out of scope: an attacker who can write to the database or read the
server's environment (they hold the secret key, and can redirect a proxy to
their own host); a compromised host or reverse proxy.

## How secrets and tokens are stored

| Secret | Stored as |
|---|---|
| Local passwords | argon2id hash (parameters in `auth.argon2`), re-hashed on login when parameters change |
| Personal, service-account and session tokens | SHA-256 of the token; the token (`hgr_…`) is shown once |
| Proxy upstream password, bearer token, header value | Sealed (envelope encryption) in `repositories.config` |
| Password in a proxy `remote_url` or `http.proxy` URL | Sealed in place of the password: `http://user:hgs1.…@host` |
| S3 secret key and session token of blob stores | Sealed in `blob_stores.config` (`secret_access_key_sealed`) |
| Server-level secrets (`storage.s3.secret_access_key`, `http.proxy_password`, `docker.token_key`, `security.secret_key`) | Not stored; read from the environment |

**Envelope encryption** (`internal/secrets`). Each value gets a fresh
256-bit data key; the value is encrypted with AES-256-GCM under the data
key, and the data key with AES-256-GCM under the key encryption key
(`security.secret_key`). The stored form is
`hgs1.<key id>.<wrapped data key>.<ciphertext>`; the key id is derived from
the key, so ids never need to be assigned. Values are sealed when a
repository or blob store is saved and opened only by the proxy engine and
the S3 driver. The API answers with the password removed and URL passwords
as `REDACTED`; saving the redacted form back keeps the stored secret.

A database check constraint (migration `00050`) rejects plaintext secrets
in every new or updated row, so no code path can store them by mistake.
Rows written before the upgrade are sealed by `hangar serve` on its first
start with a key; until a key is configured they keep working, and the
server logs a warning on every start.

Without `security.secret_key`, creating or updating a repository or blob
store that carries a credential answers `501` with an explanation.

### Key rotation

1. Generate a key: `openssl rand -base64 32`.
2. On every node set `HANGAR_SECRET_KEY=<new>` and
   `HANGAR_SECURITY_PREVIOUS_SECRET_KEYS=<old>`; restart the nodes.
3. Run `hangar admin rotate-secrets` once (`--dry-run` shows the counts). It
   re-wraps every data key under the new key and seals any plaintext left;
   it exits non-zero if a value cannot be opened with the configured keys.
4. Remove the old key from `previous_secret_keys`; restart.

A lost key cannot be recovered: re-enter the affected credentials.

## Hardening checklist for operators

- [ ] Serve Hangar over HTTPS only. Terminate TLS at a reverse proxy, list
      it in `server.trusted_proxies` so `X-Forwarded-Proto` is believed, and
      set `server.base_url` to the `https://` URL. HSTS is then sent
      (`security.hsts`); enable `include_subdomains` only when every
      subdomain is HTTPS.
- [ ] Set `HANGAR_SECRET_KEY` (and keep a copy in your secret manager) before
      adding proxy credentials or S3 blob stores. Same value on every node.
- [ ] Set `HANGAR_DOCKER_TOKEN_KEY` (32+ random bytes), same on every node.
- [ ] Single sign-on ([guide](guides/sso.md)): keep `auth.oidc.client_secret`
      and `auth.ldap.bind_password` in the environment, use `ldaps://` or
      `start_tls` (plain `ldap://` sends passwords in clear text), never
      `insecure_skip_verify` (use `ca_file`), give the LDAP service account
      read-only rights, and restrict sign-in with `allowed_groups` when the
      provider serves more people than should use Hangar. External sign-ins
      never take over a local account with the same username.
- [ ] Keep secrets in the environment or a secret store, never in
      `hangar.yaml` (`storage.s3.secret_access_key`, `http.proxy_password`,
      `bootstrap.admin_password`).
- [ ] After the first start, remove `HANGAR_BOOTSTRAP_ADMIN_PASSWORD` from the
      environment and sign in to change the bootstrap password.
- [ ] Give CI tokens the narrowest scopes (`repo:<name>:write`) and an
      expiry; prefer service accounts over personal tokens.
- [ ] Leave `anonymous_read` off except for repositories that are meant to
      be public.
- [ ] Keep `http.allow_private_networks` as short as possible: list the
      internal mirrors by host name or exact CIDR, never whole private ranges.
      Avoid `insecure_skip_verify` on remotes; use `ca_bundle` instead.
- [ ] Expose only the HTTP port. Do not expose the embedded NATS listener
      (`nats.listen`) or PostgreSQL to clients; restrict `/metrics` at the
      reverse proxy if it should not be public.
- [ ] Use TLS to PostgreSQL (`sslmode=verify-full`) and to S3 when they are
      on another host; encrypt database backups — they contain password
      hashes and sealed secrets.
- [ ] Ship the audit log off the host (export or syslog/webhook) and alert on
      repeated `auth.login.failed` and `auth.token.failed`.
- [ ] Review `security.rate_limit` for clients behind one NAT address (all
      share one budget) and `security.limits.upload_body_bytes` if uploads
      should be capped.
- [ ] Run the container image as shipped (distroless, non-root) and keep it
      updated.

### HTTP hardening reference

| Concern | Behaviour | Setting |
|---|---|---|
| Security headers | `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY` everywhere; CSP per area: UI (`default-src 'self'`, no inline script), API (`default-src 'none'`, `Cache-Control: no-store`), package endpoints (`sandbox`) | — |
| HSTS | On HTTPS requests | `security.hsts.*` |
| CORS | Same origin only; preflights and API calls from other origins answer 403 | `security.cors.allowed_origins` |
| Anonymous API access | Only `GET /system/status`, `GET /system/version`, `GET /auth/providers`, `POST /auth/login` (`security: []` in `api/openapi.yaml`) | — |
| Request headers | Bounded; slow headers cut off | `security.limits.max_header_bytes`, `server.read_header_timeout` |
| Request bodies | API 1 MiB, uploads unlimited and streamed, everything else 64 KiB; a body that stalls is aborted | `security.limits.*` |
| Rate limits | Per client IP (IPv6 per /64): API requests; failed authentications on API and package endpoints | `security.rate_limit.*`, `auth.login_rate` |
| Idle connections | Closed | `server.idle_timeout` |

## Security tests

`test/security` runs against a real `hangar serve` (it needs
`HANGAR_TEST_DATABASE_URL`, as in CI):

- **Authorization sweep**: every operation in `api/openapi.yaml` and every
  package endpoint of every format is called as an anonymous caller, as a
  reader and with a token scoped to other repositories, against private
  hosted repositories and a group and a proxy over them. Any answer that
  serves a caller who must be refused fails the build. An API operation or
  format route that is not classified in the test fails it too.
- Privilege escalation through tokens, roles and the registry token service.
- SSRF: remotes at loopback, `169.254.169.254`, private and IPv6-mapped
  addresses are refused; rows that bypass that check are blocked when
  fetched; redirects to internal addresses or other schemes are not followed.
- Upstream credentials: stored sealed, never returned, sent only to the
  remote, not to redirect targets.
- Headers, CORS and size limits on the running server.

## Static analysis

| Tool | Result (this release) |
|---|---|
| `golangci-lint` (includes gosec) | 0 issues |
| `gosec ./...` (standalone) | 40 findings, each annotated in the code with `//nolint:gosec` and its reason (standalone gosec does not read that annotation): MD5/SHA-1 are protocol checksums (Maven, npm, PyPI, raw), not security controls; integer conversions are bounded; redirects stay on the request's own path; `InsecureSkipVerify` is an explicit, audited per-remote option; file paths come from the operator or from validated digests; error bodies are `text/plain` with `nosniff`. |
| `govulncheck ./...` | No vulnerable code reached. The module-level advisory GO-2026-5932 concerns `golang.org/x/crypto/openpgp`, which Hangar does not import. |
| `npm audit --omit=dev` (web) | 0 vulnerabilities in what ships. 14 moderate advisories remain in development-only tooling (ESLint and `openapi-typescript` through `minimatch`), which never runs in production. |
