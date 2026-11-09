# Troubleshooting

Start with the three views every install has:

```bash
curl -s https://hangar.example.com/readyz          # which dependency fails
curl -s https://hangar.example.com/api/v1/system/status
docker compose logs --tail 100 hangar               # or kubectl logs / journalctl -u hangar
```

Logs are JSON lines; every request line carries `request_id` (also returned
in the `X-Request-Id` response header) and, with tracing, `trace_id`. Set
`log.level: debug` temporarily for probe and scrape requests and more detail.
Alert-specific runbooks are in [operations/runbooks.md](../operations/runbooks.md).

## Start-up

| Symptom | Cause | Fix |
|---|---|---|
| `config file ... not found` / `parse ...: field xyz not found` | Wrong path, or an unknown or misspelled key (Hangar rejects unknown keys). | Compare with `hangar.example.yaml`. |
| `database.url (HANGAR_DATABASE_URL) is required` | No database configured. | Set `HANGAR_DATABASE_URL`. |
| `database schema is out of date: run hangar migrate up` | New version, migrations not applied, `migrate_on_start: false`. | `hangar migrate up` once (Helm: check the `hangar-migrate` Job), then start. |
| `storage.gc.grace_period ... must be longer than 24h` | Grace period too short for Docker upload sessions. | Use more than `24h` (default `48h`). |
| `bootstrap.admin_user and bootstrap.admin_password must be set together` | Only one of the two set. | Set both or neither. |
| A `security.secret_key` error | Key not 32 bytes in base64 or hex. | `openssl rand -base64 32`. |
| `connect nats: ... i/o timeout` | External NATS unreachable or not ready yet. | Check `nats.url`, network policy, NATS pods. The Helm chart waits for its bundled NATS in an init container. |
| `create job stream: ... stream is offline` | The NATS server holding the job stream is down or restarting (single-replica stream). | Bring the NATS server back; Hangar restarts until the stream is available. Set `nats.replicas: 3` on a 3-node cluster ([high-availability.md](high-availability.md#job-queue-replicas)). |
| `update job stream HANGAR_JOBS (replicas 1 to 5): ... no suitable peers for placement` | `nats.replicas` is higher than the number of NATS servers. | Lower `nats.replicas` to at most the cluster size. |
| `nats.replicas 3: embedded NATS is a single server` | Replicas above 1 with `nats.embedded: true`. | Use 1, or an external NATS cluster. |
| Start hangs after `waiting for the migration lock: another process is applying migrations` | Another node or `hangar migrate up` is migrating the same database. | Wait; it continues when that migration finishes. If nothing should be migrating, find the session holding the lock (connected to Hangar's database and schema): `SELECT a.pid, a.application_name, a.client_addr, a.backend_start FROM pg_locks l JOIN pg_stat_activity a USING (pid) WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1 AND ((l.classid::bigint << 32) \| l.objid::bigint) = hashtextextended('hangar.migrations:' \|\| current_schema(), 0)`. |
| `metrics.auth=bearer needs metrics.bearer_token ...` | `metrics.auth` set without its credentials (or shorter than 16 characters). | Set `HANGAR_METRICS_BEARER_TOKEN` (or `metrics.bearer_token_file`); for `basic`, the username and password. |
| Helm: `execution error ... config.server.base_url is required` (or another chart message) | The chart validates values before rendering. | Follow the message; [install-helm.md](install-helm.md). |
| Container exits with permission errors on `/var/lib/hangar` | Volume owned by another UID. | The image runs as 65532; `chown -R 65532:65532` the volume, or let Kubernetes `fsGroup: 65532` handle it. |

## Readiness (`/readyz` answers 503)

The body names the failing check:

| Check | Typical causes |
|---|---|
| `database` | PostgreSQL down, `max_connections` exhausted (`nodes x (max_conns + 16)`), credentials changed. |
| `blobstore` | S3 endpoint unreachable, wrong credentials or bucket, bucket policy missing an action; filesystem full or read-only. |
| `eventbus` | NATS connection lost, JetStream disabled or lost quorum. |

During shutdown a node answers 503 on purpose (draining).

## Clients

| Symptom | Cause | Fix |
|---|---|---|
| Large uploads fail after exactly 60 s, or with 413 / `request entity too large` | The reverse proxy limits body size or time (Traefik's 60 s `readTimeout`, an L7 load balancer's idle timeout, an ingress body limit). | [reverse-proxy.md](reverse-proxy.md). |
| Uploads are slow to start, then proxy disk fills | The proxy buffers request bodies. | Turn request buffering off. |
| `docker login` loops or reports `unauthorized` although the password is right | `server.base_url` is wrong: the token realm in the 401 points to a host or scheme the client cannot reach. | Set `server.base_url` to the public HTTPS URL; check `curl -sI https://<host>/v2/` for the `WWW-Authenticate` realm. |
| `docker push` to `registry.example.com` fails on the token request | The registry host is reachable but the UI host (realm) is not. | Make `server.base_url` reachable from Docker clients too. |
| Every client gets 429 at once | All clients appear with the proxy's IP and share one rate-limit bucket. | Set `server.trusted_proxies` to the proxy's address. |
| 429 for one client with valid credentials | That IP had many failed authentications (`security.rate_limit.failed_auth`), e.g. a CI job with an expired token. | Fix the client's credentials; the bucket refills within minutes. |
| `423 Locked` on sign-in | This address is locked out of the username after repeated wrong passwords (`auth.lockout`). | Wait 15 minutes, sign in from another address, or unlock it as an administrator. |
| Server does not start: `... is the public development key from deploy/docker-compose.yml` | The configuration uses a key published for development. | Generate your own: `openssl rand -base64 48` for `docker.token_key`; `openssl rand -base64 32` for `security.secret_key`, keeping the old key in `security.previous_secret_keys` until `hangar admin rotate-secrets` has run. `server.dev_mode: true` is for development servers only. |
| Browser sign-in does not stick over plain HTTP | Session cookies are `Secure`. | Use HTTPS. Only for a lab on plain HTTP (not localhost), `auth.session.insecure: true`. |
| Links in npm metadata or the UI point at `localhost` or the wrong host | `server.base_url` not set. | Set it; generated links never use the request's `Host`. |
| `501` when saving a proxy repository with credentials or a blob store with keys | No `security.secret_key`. | Set `HANGAR_SECRET_KEY` on every node and restart ([security.md](../security.md)). |
| Proxy repository shows offline; logs `x509: certificate signed by unknown authority` | TLS-inspecting corporate proxy or private CA. | `http.ca_file` ([corporate-proxy.md](corporate-proxy.md)). |
| Proxy repository to an internal host is refused | Outbound address policy (SSRF guard). | `http.allow_private_networks`. |
| Proxy repository shows *authentication failed (401)* or *access denied (403)* | The remote refuses the repository's upstream credentials (wrong, expired or missing token; too few rights). | Update the credentials in the repository's settings. |
| Groups answer 502 during CI bursts; logs show DNS `i/o timeout` | The DNS servers time out under the load. | Keep `http.dns.cache` on (default) and watch `hangar_upstream_dns_errors_total` ([corporate-proxy.md](corporate-proxy.md#dns-cache)). |
| `/metrics` reachable from the Internet | The public host routes everything to Hangar. | Block it at the proxy (the shipped Caddy, Traefik and Helm examples do). |

## Storage

| Symptom | Cause | Fix |
|---|---|---|
| Disk or bucket keeps growing after deletes | Blob GC keeps unreferenced blobs for `storage.gc.grace_period` and runs daily on the leader. | Wait for the next GC run, or check the task list for failed `blob.gc` runs. Cleanup policies decide what gets deleted in the first place. |
| S3 `AccessDenied` on uploads only | Missing `s3:PutObject` or multipart actions. | Grant the actions in [requirements.md](requirements.md). |
| An upload fails with `blob sha256:... was reclaimed by garbage collection during the publish; upload it again` | The content was identical to an unreferenced blob that `blob.gc` deleted while the upload was being published. Hangar refuses the publish rather than store a reference to deleted bytes. | Retry the upload; it stores the bytes again. Rare, and only while `blob.gc` runs. |
| After a restore, some downloads fail | Blobs copied before the database dump, or later than the GC grace period. | Restore blobs from a later copy; see [backup-restore.md](backup-restore.md). |

## Getting more detail

- `log.level: debug` (or `HANGAR_LOG_LEVEL=debug`) and restart.
- Tracing: `observability.tracing.enabled: true` with an OTLP collector, then
  follow the `trace_id` from the log line ([monitoring.md](../operations/monitoring.md)).
- Configuration as loaded: Hangar logs `starting hangar` with the address,
  base URL, storage kind and NATS mode; secrets are never logged.
