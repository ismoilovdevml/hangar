# Alert runbooks

One section per alert in `deploy/observability/prometheus/hangar-alerts.yml`.
Each says what the alert means, what users notice, how to find the cause and
how to fix it. Metric and log references are in [monitoring.md](monitoring.md).

Useful everywhere:

- `curl -s http://<node>:8080/readyz` shows each dependency check with its error.
- Logs are JSON; filter by `request_id` or `trace_id` to follow one request,
  and by `"level":"ERROR"` for failures.
- The Hangar dashboards (`deploy/observability/grafana/`) cover each area.

## HangarDown

**Meaning.** Prometheus has not been able to scrape the node for 2 minutes.

**Impact.** If it is the only node, every client fails. Behind a load balancer
the other nodes take the traffic if their readiness passes.

**Diagnose.**
1. Is the process running? (`systemctl status hangar`, `kubectl get pods`,
   `docker compose ps`). Look at the last log lines for a fatal error such as
   `database schema is out of date` or a configuration error.
2. If it runs, does it answer locally? `curl -s localhost:8080/healthz`.
   No answer while running points to a hung process or exhausted file descriptors
   (`process_open_fds`).
3. If it answers locally, the problem is between Prometheus and the node
   (firewall, DNS, wrong target, `metrics.enabled: false`).

**Fix.** Restart the node after capturing its logs. Fix the configuration error
it reports; for a pending migration run `hangar migrate up`.

## HangarNotReady

**Meaning.** The node's readiness (`hangar_ready`, the same check as `/readyz`)
has failed for 5 minutes, so load balancers stop sending it traffic.

**Impact.** Reduced capacity; total outage if every node is not ready.

**Diagnose.** `curl -s http://<node>:8080/readyz` lists the failing check;
`hangar_dependency_up` shows the same per dependency. A `draining` status means
the node is shutting down; it clears when the new process starts.

**Fix.** Follow [HangarDependencyDown](#hangardependencydown) for the failing
dependency. A node stuck draining for more than `server.shutdown_timeout` is
hung on a long upload: restart it.

## HangarDependencyDown

**Meaning.** One readiness check (`database`, `blobstore` or `eventbus`) has
failed for 2 minutes.

**Impact.** Every operation that needs the dependency fails; the node is not
ready.

**Diagnose and fix.**
- `database`: can the node reach PostgreSQL (`psql "$HANGAR_DATABASE_URL" -c 'select 1'`)?
  Check PostgreSQL logs, `max_connections`, disk space and failover state.
- `blobstore`: for `fs`, the directory must exist and be writable by the hangar
  user (a full or read-only filesystem fails the check). For `s3`, check
  credentials, bucket policy, endpoint reachability and certificate.
- `eventbus`: with external NATS check the cluster through its monitoring
  port (`curl http://<nats>:8222/healthz?js-enabled-only=true`);
  `hangar_nats_connected` shows the node's connection. With embedded NATS check that `nats.data_dir` is
  writable and not full.

## HangarHigh5xxRate

**Meaning.** More than 5% of the node's requests (at least 0.1 per second) have
failed with a server error for 10 minutes. 501 (not implemented) is excluded.

**Impact.** Clients see failed pulls, pushes or API calls.

**Diagnose.**
1. Overview dashboard → *Server errors by route*: which routes and codes.
2. `502` and `504` on `/repository/{repo}/*` usually mean proxy remotes fail:
   check [HangarUpstreamCircuitOpen](#hangarupstreamcircuitopen) and the
   Upstreams dashboard. A 502 from a remote is visible to the client even when
   Hangar itself is healthy.
3. `500`: search the logs for `"level":"ERROR"` and take the `request_id`/
   `trace_id` of a failing request; the trace shows which database statement,
   blob store operation or upstream fetch failed.
4. `503`: a repository is offline or the node is draining.

**Fix.** Fix the failing dependency; for a proxy remote, see below. If one
release introduced the errors, roll back.

## HangarUpstreamCircuitOpen

**Meaning.** A proxy repository's remote failed repeatedly (connection errors
or 5xx answers) and its circuit breaker is open: Hangar stops calling it for
the breaker's open period and then probes again.

**Impact.** Content already cached in the repository is still served. Content
not yet cached fails with 502 until the remote recovers. Groups fall through to
their other members.

**Diagnose.**
1. Upstreams dashboard → *Remotes not healthy* and *Fetches by result*.
2. Node log: `"msg":"upstream request failed"` with the repository and error
   (DNS, TLS, timeout, 5xx).
3. From the node, try the remote directly through the same outbound proxy:
   `curl -I --proxy "$HANGAR_HTTP_PROXY" <remote_url>`.

**Fix.** Wait out a remote outage. Fix outbound proxy, firewall, DNS or CA
settings (`http.*` or the repository's `proxy`/`ca_bundle` overrides). If the
remote moved, update the repository's remote URL; the breaker resets with the
new configuration.

## HangarBlobStoreErrors

**Meaning.** More than 5 blob store operations of one kind (`put`, `get`,
`stat`, `delete`) failed on a node in 10 minutes. `not_found` answers are not
errors.

**Impact.** `put` errors fail uploads and proxy cache fills; `get` errors fail
downloads.

**Diagnose.**
- `fs`: disk full (`df -h <storage.fs.path>`), read-only remount, permission
  changes, failing NFS mount. Kernel log (`dmesg`) for I/O errors.
- `s3`: node log for the S3 error code. `AccessDenied` → credentials or bucket
  policy; `SlowDown`/`503` → throttling; TLS or DNS errors → endpoint.
- Storage dashboard → *Errors* and *Latency* by driver and operation.

**Fix.** Free or add space, restore permissions or credentials, or fail over
the object store. Uploads that failed must be retried by the client.

## HangarDatabasePoolSaturated

**Meaning.** Over 90% of the node's database connections (`database.max_conns`)
have been in use for 10 minutes.

**Impact.** Requests wait for a connection; latency grows and requests may time
out.

**Diagnose.**
1. Overview dashboard → *Pool wait* and *Statements (p95, errors)*: slow
   statements hold connections longer.
2. In PostgreSQL: `SELECT pid, state, wait_event, now() - query_start AS age, left(query, 120)
   FROM pg_stat_activity WHERE application_name = 'hangar' ORDER BY age DESC;`
   Look for long-running queries and lock waits.

**Fix.** Kill runaway queries, fix locks, then consider raising
`database.max_conns` (stay below PostgreSQL's `max_connections` divided by the
node count) or adding nodes.

## HangarJobFailures

**Meaning.** A background job failed on its last attempt (5 attempts with
backoff) and was dropped.

**Impact.** Depends on the job: `blob.gc` → storage is not reclaimed;
`audit.ensure_partitions` → audit writes fail when the month rolls over;
`auth.purge_expired` → expired tokens and sessions accumulate.

**Diagnose.** Node log: `"msg":"job failed permanently"` with `type`, `job_id`
and the error; earlier `"job failed, will retry"` lines show the history. Jobs
dashboard → *Executions by result*.

**Fix.** Fix the cause (database, blob store, permissions). Scheduled jobs run
again on their next schedule; for `audit.ensure_partitions` near a month
boundary, restart the leader node to schedule it promptly.

## HangarJobBacklog

**Meaning.** More than 100 jobs of one type have been queued for 30 minutes.

**Impact.** Background work (scans, cleanup) runs late.

**Diagnose.** Jobs dashboard: is anything executing (*Executions by result*)?
`hangar_job_queue_up` and `hangar_nats_connected` show whether nodes can reach
the queue. Repeated `retry` results mean jobs keep failing; long durations mean
workers are slow.

**Fix.** Restore NATS connectivity or the failing dependency. Make sure at
least one node runs (every node executes jobs). If a bug enqueues duplicates,
fix it before purging the stream.

## HangarBlobStoreQuotaNearLimit

**Meaning.** A blob store uses more than 90% of its soft quota.

**Impact.** None yet; it is a capacity warning before the store fills.

**Diagnose.** Storage dashboard → *Blob stores*, *Used and reclaimable*.
Reclaimable bytes are freed by the next `blob.gc` run. The Blob stores screen
shows growth per day.

**Fix.** Run cleanup policies, delete unused components, let GC run, raise the
soft quota or move repositories to another store.

## HangarBlobStoreDiskNearlyFull

**Meaning.** The filesystem holding an `fs` blob store has had less than 10%
free space for 15 minutes.

**Impact.** When it fills, every upload and proxy cache fill fails, and the
embedded NATS store may fail too if it shares the disk.

**Diagnose.** `df -h <storage.fs.path>`; the Storage dashboard → *Filesystem
free*. Check whether other data (logs, temporary files under the store's
`tmp/`) shares the filesystem.

**Fix.** Grow the volume, free space, run cleanup and GC, or move blobs to a
larger store.

## HangarStorageUsageStale

**Meaning.** The background job that computes blob store usage has not
succeeded for over an hour, so the quota and disk alerts look at old numbers.

**Diagnose.** Node log: `"msg":"blob store usage for metrics"` with the error,
usually a database timeout on a very large `blobs` table or a database outage.

**Fix.** Fix database connectivity or performance. The computation retries
every 5 minutes on its own.

## HangarAuthBruteForce

**Meaning.** More than 50 authentications failed on a node within 5 minutes,
sustained for 5 minutes.

**Impact.** Possible credential guessing; affected accounts get locked.

**Diagnose.**
1. Auth dashboard → *Failures by reason*: `unknown_user` across many names is
   guessing; `bad_password` on one user is often a CI job with a stale password;
   `unknown_expired_or_revoked` is a client with an old token.
2. Audit log (`GET /api/v1/audit?action=auth.login.failed`, or the Audit screen)
   shows actor and source IP of each attempt.

**Fix.** Block abusive source addresses at the firewall or reverse proxy.
Rotate credentials of the affected CI clients. Make sure
`server.trusted_proxies` is set so audit records show real client IPs.

## HangarAccountLockouts

**Meaning.** More than 3 accounts were locked in 15 minutes by the failed-login
limit (`auth.lockout`).

**Impact.** Locked users and CI jobs cannot authenticate until the lock expires
or an administrator unlocks them.

**Diagnose.** Audit log entries `auth.login.failed` with `reason=bad_password`
and `locked_until`: which users, from which IPs.

**Fix.** If it is an attack, block the source and leave the locks. If it is a
misconfigured client, fix its credentials; the locks expire after
`auth.lockout.duration`, and an administrator can end one earlier by setting
the user's password.

## HangarAuditEventsDropped

**Meaning.** Audit events were rejected because the audit queue stayed full
(`hangar_audit_events_rejected_total`; the mutation that produced them failed
with an error) or could not be written after every retry
(`hangar_audit_events_lost_total`; they were written to the log as
`"msg":"audit event not stored"`).

**Impact.** The audit trail in PostgreSQL is incomplete.

**Diagnose.** Node log for `audit batch insert failed` and its error: a
database outage, a missing monthly partition of `audit_events` (see
[HangarJobFailures](#hangarjobfailures) for `audit.ensure_partitions`) or a slow
database. Auth dashboard → *Audit queue*.

**Fix.** Restore the database or create the missing partition (run the
`audit.ensure_partitions` job by restarting the leader). Recover lost events
from the `audit event not stored` log lines if your compliance rules require a
complete trail.
