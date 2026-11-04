# Monitoring Hangar

Hangar exposes Prometheus metrics, writes structured JSON logs and exports
OpenTelemetry traces. This page is the reference for all three and for the
dashboards and alerts shipped in `deploy/observability/`. Alert runbooks are in
[runbooks.md](runbooks.md).

## Quick start

1. **Metrics** are served at `/metrics` on every node (`metrics.enabled`,
   `metrics.path`). Scrape each node with a job named `hangar`; see
   `deploy/observability/prometheus/prometheus.example.yml`. `/metrics` has no
   authentication: keep it on an internal network or block it at the reverse
   proxy.
2. **Alerts**: load `deploy/observability/prometheus/hangar-alerts.yml` as a
   rule file. Check and unit-test them with
   `promtool check rules hangar-alerts.yml` and
   `promtool test rules hangar-alerts.test.yml`.
3. **Dashboards**: import the JSON files in `deploy/observability/grafana/`
   (Dashboards → New → Import) and pick your Prometheus data source. They share
   the `hangar` tag and link to each other; the `Node` variable filters by
   instance.
4. **Traces**: set `observability.tracing.enabled: true` and
   `observability.tracing.endpoint` to an OTLP/HTTP collector (see below).
5. **Logs**: ship the process's stderr (JSON lines) to your log system and
   index `request_id` and `trace_id`.

## Configuration

| Setting | Env | Default | Meaning |
|---|---|---|---|
| `metrics.enabled` | `HANGAR_METRICS_ENABLED` | `true` | Serve the Prometheus endpoint. Metrics are recorded either way (the Overview screen reads them). |
| `metrics.path` | `HANGAR_METRICS_PATH` | `/metrics` | Endpoint path. |
| `log.level` | `HANGAR_LOG_LEVEL` | `info` | `debug` also logs probe and scrape requests. |
| `log.format` | `HANGAR_LOG_FORMAT` | `json` | `json` or `text`. |
| `observability.access_log.sample_ratio` | `HANGAR_OBSERVABILITY_ACCESS_LOG_SAMPLE_RATIO` | `1` | Share of requests written to the access log, 0 to 1. Server errors (5xx) are always logged. Metrics and traces are not sampled by it. |
| `observability.tracing.enabled` | `HANGAR_OBSERVABILITY_TRACING_ENABLED` | `false` | Export traces. |
| `observability.tracing.endpoint` | `HANGAR_OBSERVABILITY_TRACING_ENDPOINT` | | OTLP/HTTP collector URL, e.g. `http://otel-collector:4318`. Without a path `/v1/traces` is used; `https://` uses TLS with the system roots. |
| `observability.tracing.sample_ratio` | `HANGAR_OBSERVABILITY_TRACING_SAMPLE_RATIO` | `0.1` | Share of new traces recorded, 0 to 1. Requests carrying a sampled W3C `traceparent` header are always recorded (parent-based sampling). |
| `observability.tracing.service_name` | `HANGAR_OBSERVABILITY_TRACING_SERVICE_NAME` | `hangar` | `service.name` of every span. |

Collector authentication headers come from the standard OpenTelemetry
variables `OTEL_EXPORTER_OTLP_HEADERS` or `OTEL_EXPORTER_OTLP_TRACES_HEADERS`
(e.g. `authorization=Bearer …`). If the collector is unreachable, export
errors are logged at most once a minute and requests are not affected.

## Metrics

All Hangar metrics start with `hangar_`. The Go runtime (`go_*`) and process
(`process_*`) collectors are also exported. Every value is **per node**:
aggregate across instances in queries.

### Cardinality

Labels never carry user-controlled values:

- HTTP metrics use the chi **route pattern** (`/repository/{repo}/*`,
  `/api/v1/components/{id}`), never the request path; requests no route
  matched are `route="unmatched"`.
- `method` is one of the standard HTTP methods or `OTHER`; `code` is the
  status code or `other` outside 100–599.
- Repository names appear only in `hangar_upstream_state` (one series per proxy
  repository and state, capped at 512 repositories) and store names only in the
  blob store usage gauges (capped at 64 stores). Both sets are created by
  administrators.
- `format`, job `type` and similar values from code or configuration are capped
  (32 formats, 64 job types); anything beyond becomes `other`.

Measured on a test node serving three formats, five repositories and a few hundred requests: 666 series in total, 622 of them `hangar_*`, most of them histogram buckets. Expect low thousands on a busy node with every format in use.

### HTTP

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hangar_http_requests_total` | counter | `method`, `route`, `code` | Requests served. |
| `hangar_http_request_duration_seconds` | histogram | `method`, `route` | Time until the handler returned (for downloads, until the last byte was written). |
| `hangar_http_requests_in_flight` | gauge | | Requests being served now, including long uploads and the event stream. |
| `hangar_http_request_bytes_total` | counter | `route` | Request body bytes read by handlers. |
| `hangar_http_response_bytes_total` | counter | `route` | Response bytes written. |

### Package formats

Recorded for requests served by a format (`/repository/{repo}/…`, `/v2/…`)
once the addressed repository is resolved. A request through a group is
attributed to the group. `op` is `read` (GET, HEAD), `write` (PUT, POST, PATCH),
`delete` or `other`; `repo_type` is `hosted`, `proxy` or `group`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hangar_format_requests_total` | counter | `format`, `repo_type`, `op`, `status_class` | Package protocol requests; `status_class` is `2xx` … `5xx`. A 401 is usually the normal login challenge of docker, npm and pip. |
| `hangar_format_request_duration_seconds` | histogram | `format`, `repo_type`, `op` | Package request latency. |
| `hangar_format_upload_bytes_total` | counter | `format`, `repo_type` | Bytes received in write requests. |
| `hangar_format_download_bytes_total` | counter | `format`, `repo_type` | Bytes sent in successful (2xx) reads. |
| `hangar_proxy_cache_requests_total` | counter | `format`, `result` | Proxy requests answered from the cache (`hit`) or fetched from the remote (`miss`). |
| `hangar_proxy_cache_hit_bytes_total` | counter | `format` | Bytes served from proxy caches without contacting the remote. |

### Blob storage

Operations are timed on the request path (uploads, downloads, publish
checks), per driver (`fs`, `s3`). Garbage collection accesses stores directly
and is measured by the `blob.gc` job metrics. `result` is `ok`, `not_found`
(a normal answer, e.g. a cache miss) or `error`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hangar_blobstore_operations_total` | counter | `driver`, `op`, `result` | Operations by `op` (`put`, `get`, `stat`, `delete`). |
| `hangar_blobstore_operation_duration_seconds` | histogram | `driver`, `op` | Latency. `put` includes streaming the content; `get` is the time to open the blob. |
| `hangar_blobstore_put_bytes_total` | counter | `driver` | Bytes written before deduplication. |
| `hangar_blobstore_used_bytes` | gauge | `store`, `driver` | Bytes of live blobs per store (as on the Blob stores screen). |
| `hangar_blobstore_reclaimable_bytes` | gauge | `store`, `driver` | Bytes of soft-deleted blobs that the next GC run frees. |
| `hangar_blobstore_soft_quota_bytes` | gauge | `store`, `driver` | Soft quota of stores that have one. |
| `hangar_blobstore_fs_size_bytes` | gauge | `store` | Size of the filesystem holding an `fs` store, as seen by this node. |
| `hangar_blobstore_fs_free_bytes` | gauge | `store` | Bytes available to Hangar on that filesystem. |
| `hangar_blobstore_usage_age_seconds` | gauge | | Age of the usage numbers above. |

Store usage is computed every 5 minutes in the background by one aggregate
query over the `blobs` table, so scrapes never wait for it. Until the first
run finishes after start the usage gauges are absent.

### Proxy remotes

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hangar_upstream_fetches_total` | counter | `format`, `result` | Requests to remotes. `result`: `ok`, `not_modified`, `not_found`, `client_error`, `server_error`, `circuit_open` (refused by the open breaker without contacting the remote), `blocked` (outbound address policy), `canceled`, `error` (connection, TLS, timeout). |
| `hangar_upstream_fetch_duration_seconds` | histogram | `format` | Time to the remote's response headers (fast `circuit_open` and `canceled` results are not observed). |
| `hangar_upstream_state` | gauge | `repository`, `state` | 1 for the current state of each proxy repository's remote on this node: `healthy`, `slow`, `blocked` (401/403 or address policy) or `offline` (circuit breaker open). Refreshed at most every 15 seconds. |

### Database

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hangar_db_query_duration_seconds` | histogram | `op` | Statement latency by leading SQL keyword: `select`, `insert`, `update`, `delete`, `with`, `copy`, `batch`, `begin`, `commit`, `rollback`, `other`. |
| `hangar_db_query_errors_total` | counter | `op` | Failed statements (cancelled requests excluded). |
| `hangar_db_pool_connections` | gauge | `state` | Pool connections that are `acquired`, `idle` or `constructing`. |
| `hangar_db_pool_max_connections` | gauge | | `database.max_conns`. |
| `hangar_db_pool_acquires_total` | counter | | Connections handed out. |
| `hangar_db_pool_empty_acquires_total` | counter | | Acquisitions that had to wait because no connection was idle. |
| `hangar_db_pool_canceled_acquires_total` | counter | | Acquisitions abandoned because the request ended first. |
| `hangar_db_pool_acquire_wait_seconds_total` | counter | | Total time spent waiting for a connection. |
| `hangar_db_pool_new_connections_total` | counter | | Connections opened. |

### Jobs and event bus

Job `type` is one of the registered job types (`blob.gc`,
`auth.purge_expired`, `audit.ensure_partitions`, and those added by later
features).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hangar_jobs_total` | counter | `type`, `result` | Executions on this node: `success`, `retry` (failed, will run again), `failed` (failed on its last attempt and was dropped). |
| `hangar_job_duration_seconds` | histogram | `type` | Execution time. |
| `hangar_job_queue_wait_seconds` | histogram | `type` | Time from enqueue to the first attempt. |
| `hangar_job_queue_depth` | gauge | `type` | Jobs waiting or running in the cluster-wide JetStream work queue (same value on every node). |
| `hangar_job_queue_up` | gauge | | 1 if the job stream could be read on this scrape. |
| `hangar_nats_connected` | gauge | | 1 while connected to NATS. |
| `hangar_nats_reconnects_total` | counter | | Reconnections to NATS. |

### Authentication and audit

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hangar_auth_failures_total` | counter | `method`, `reason` | Failed authentications. `method`: `login` (web sign-in), `basic` (HTTP Basic on API or package requests), `token`. `reason`: `bad_password`, `unknown_user`, `locked`, `disabled`, `no_local_password`, `malformed`, `unknown_expired_or_revoked`. |
| `hangar_auth_lockouts_total` | counter | | Accounts locked by the failed-login limit (`auth.lockout`). |
| `hangar_audit_queue_length` | gauge | | Audit events waiting to be written to PostgreSQL. |
| `hangar_audit_queue_capacity` | gauge | | Size of the in-memory audit queue. |
| `hangar_audit_events_rejected_total` | counter | | Events refused because the queue stayed full; the mutation that produced them failed. |
| `hangar_audit_events_lost_total` | counter | | Events that could not be stored after every retry; they were written to the log instead. |

### Health and build

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hangar_ready` | gauge | | 1 when `/readyz` would answer 200 (all checks pass, not draining). Checks run at most every 5 seconds. |
| `hangar_dependency_up` | gauge | `dependency` | Result of each readiness check: `database`, `blobstore`, `eventbus`. |
| `hangar_build_info` | gauge | `version`, `commit`, `goversion` | Always 1. |

## Logs

Logs are JSON lines on stderr (`log.format: text` for development). Every
record logged while serving a request carries:

| Field | Meaning |
|---|---|
| `request_id` | `X-Request-ID` of the request: the client's value when it is well-formed (letters, digits, `-_.`, at most 128 characters), otherwise generated; echoed in the response. |
| `trace_id`, `span_id` | Current span when tracing is enabled or the client sent a `traceparent`. Search your trace backend by `trace_id`. |
| `principal` | Authenticated user or service account. |
| `repository`, `format` | Repository the request addressed (a group, not its member) and its format. |

The access log line (`"msg":"http request"`) adds `method`, `path` (without the
query string), `route`, `status`, `bytes_in`, `bytes_out`, `duration_ms`,
`remote_ip` and `user_agent`. It is written at `info`, `error` for 5xx (except
501), and `debug` for `/healthz`, `/readyz` and the metrics path.

**Secrets are never logged.** Before a record is written, values of attributes
whose name denotes a secret (`password`, `token`, `secret`, `authorization`,
`cookie`, `*_password`, `*_token`, `*_secret`, …) are replaced with
`REDACTED`, and every message and string value is scrubbed of Hangar tokens
(`hgr_…`), JWTs, `Basic`/`Bearer` credentials, passwords in URLs and secret
query parameters (`token=`, `password=`, `X-Amz-Signature=`, `_state=`, …). A
test feeds credentials through every logging path and fails if any of them
reaches the output.

## Traces

With tracing enabled, Hangar exports these spans over OTLP/HTTP:

| Span | Kind | Attributes |
|---|---|---|
| `GET /repository/{repo}/*` (method + route) | server | `http.request.method`, `http.route`, `http.response.status_code`, `url.path`, `client.address`, `user_agent.original`, `hangar.request_id`, `hangar.repository`, `hangar.format`, `hangar.repository_type`, `enduser.id` |
| `db select`, `db insert`, … `db batch`, `db copy` | client | `db.system.name`, `db.operation.name`, `db.query.text` (parameterised SQL, at most 2 KiB; argument values are never recorded) |
| `blobstore.put` / `get` / `stat` / `delete` | client | `hangar.blobstore.driver`, `hangar.blob.size` |
| `upstream.fetch` | client | `hangar.repository`, `hangar.format`, `http.request.method`, `hangar.upstream.path`, `hangar.upstream.result`, `http.response.status_code` |
| `job <type>` | consumer | `hangar.job.type`, `hangar.job.id`, `hangar.job.attempt` |

Incoming W3C `traceparent`/`tracestate` headers are honoured, so a CI system or
reverse proxy that traces requests sees Hangar's spans in the same trace.
Hangar does not send trace headers to proxy remotes. Jobs start new traces.

## Dashboards

| File | Answers |
|---|---|
| `hangar-overview.json` | Is every node ready? How much traffic, how many errors, how slow? Is the database pool saturated? |
| `hangar-formats.json` | Which formats carry the traffic, fail or are slow? How many bytes go in and out? How well do proxy caches work? |
| `hangar-storage.json` | How full are the blob stores and their filesystems? Are blob store operations failing or slow? |
| `hangar-upstreams.json` | Which proxy remotes are offline, blocked or slow? How often do fetches fail and how long do they take? |
| `hangar-jobs.json` | Is the job queue draining? Which jobs fail or take long? Is NATS connected? |
| `hangar-auth.json` | Is someone guessing passwords? Are accounts being locked? Is the audit trail complete? |

The dashboards use `$__rate_interval`, so set the data source's scrape interval
in Grafana to match Prometheus.

## Alerts

`deploy/observability/prometheus/hangar-alerts.yml`:

| Alert | Severity | Fires when |
|---|---|---|
| [HangarDown](runbooks.md#hangardown) | critical | A node has not been scrapable for 2 minutes. |
| [HangarNotReady](runbooks.md#hangarnotready) | critical | A node has failed readiness for 5 minutes. |
| [HangarDependencyDown](runbooks.md#hangardependencydown) | critical | A readiness dependency has been down for 2 minutes. |
| [HangarHigh5xxRate](runbooks.md#hangarhigh5xxrate) | critical | More than 5% of a node's requests fail with 5xx for 10 minutes. |
| [HangarUpstreamCircuitOpen](runbooks.md#hangarupstreamcircuitopen) | warning | A proxy remote's circuit breaker has been open for 5 minutes. |
| [HangarBlobStoreErrors](runbooks.md#hangarblobstoreerrors) | critical | More than 5 blob store operations of one kind failed in 10 minutes. |
| [HangarDatabasePoolSaturated](runbooks.md#hangardatabasepoolsaturated) | warning | Over 90% of the pool has been in use for 10 minutes. |
| [HangarJobFailures](runbooks.md#hangarjobfailures) | warning | A job failed permanently in the last hour. |
| [HangarJobBacklog](runbooks.md#hangarjobbacklog) | warning | More than 100 jobs of a type have been queued for 30 minutes. |
| [HangarBlobStoreQuotaNearLimit](runbooks.md#hangarblobstorequotanearlimit) | warning | A store has used over 90% of its soft quota for 15 minutes. |
| [HangarBlobStoreDiskNearlyFull](runbooks.md#hangarblobstoredisknearlyfull) | critical | An `fs` store's filesystem has had under 10% free for 15 minutes. |
| [HangarStorageUsageStale](runbooks.md#hangarstorageusagestale) | warning | Store usage has not been refreshed for over an hour. |
| [HangarAuthBruteForce](runbooks.md#hangarauthbruteforce) | warning | More than 50 failed authentications in 5 minutes, sustained for 5 minutes. |
| [HangarAccountLockouts](runbooks.md#hangaraccountlockouts) | warning | More than 3 accounts locked in 15 minutes. |
| [HangarAuditEventsDropped](runbooks.md#hangarauditeventsdropped) | critical | Any audit event was rejected or lost. |
