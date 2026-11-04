# Performance

What one Hangar node handles, how it was measured, and what limits it. The
load scenarios are in [test/load](../../test/load/README.md); reproduce any
number below with `test/load/run.sh <scenario>`.

## Test setup

| | |
|---|---|
| Machine | Apple M4 Pro, 12 cores, 24 GiB RAM, internal SSD, macOS 26 (Darwin 25.6) |
| Hangar | one node, `database.max_conns: 20`, filesystem blob store on the same SSD, embedded NATS, log level `warn` |
| PostgreSQL | 17.11 on the same machine, default settings |
| Load generator | k6 2.3.0 on the same machine, 32 virtual users, 30 s per scenario, keep-alive connections, bearer token auth |
| Data | the scenario fixtures plus 12 seeded repositories with 99 996 components and 999 960 assets (`cmd/hangar-seed`) |

The machine was not isolated: other workloads ran during the measurements,
and client, server and database share 12 cores. Expect about 10-20 % variance
between runs. Production numbers depend mainly on the database and on the
disk's flush latency (see writes).

## Results

32 virtual users, 30 s, measured traffic only (fixtures published by each
scenario's setup are excluded), on the build of this branch rebased onto
`main` (with sign-in, rate limiting and the other security changes active).
No request failed and every threshold passed.

| Scenario | Requests/s | p95 | p99 | Notes |
|---|---:|---:|---:|---|
| raw downloads (1 KiB to 1 MiB) | 19 112 | 2.5 ms | 3.6 ms | 1 MiB files: p95 3.3 ms |
| docker blob GETs (4 KiB to 1 MiB) | 22 279 | 2.5 ms | 3.9 ms | registry bearer token |
| npm metadata and tarballs | 14 144 | 3.0 ms | 3.8 ms | 100-version full packument: p95 3.7 ms |
| maven metadata, checksums, POMs, jars | 28 302 | 1.7 ms | 2.3 ms | `maven-metadata.xml`: p95 1.0 ms |
| mixed, reads (90 %) | 1 135 in total | 6.1 ms | 19.5 ms | |
| mixed, writes (10 %) | | 474 ms | 590 ms | npm publish p95 553 ms, Maven deploy 202 ms, raw upload 224 ms, dist-tag 481 ms |
| seeded metadata reads | 764 | 158 ms | 197 ms | dominated by NuGet search, see below |

Single endpoints on the seeded data, 32 concurrent connections (`oha`, 8 s;
measured before the rebase onto `main`):

| Request | Requests/s | p95 |
|---|---:|---:|
| `maven-metadata.xml` of an artifact in a repository with 8 333 components | 52 585 | 0.9 ms |
| raw directory listing (JSON) in a repository with 83 330 assets | 19 258 | 2.6 ms |
| npm packument lookup in a repository with 83 330 assets (answers 404: seeded rows have no manifests, so this measures the lookups) | 17 245 | 3.9 ms |
| NuGet search whose first word matches every package (`q=pkg-505`) | 165 | 252 ms |

### Compared with the previous build

The same scenarios against the build this work started from (`237ab95`,
same data, same machine). The single-endpoint rows compare `oha` runs.

| Scenario | Before: req/s, p95 | After: req/s, p95 |
|---|---|---|
| raw | 14 482, 3.2 ms | 19 112, 2.5 ms |
| docker | 17 908, 3.1 ms | 22 279, 2.5 ms |
| npm | 2 344, 57.8 ms (100-version packument: 99 ms) | 14 144, 3.0 ms (3.7 ms) |
| maven | 9 164, 7.4 ms | 28 302, 1.7 ms |
| seeded npm packument lookup | 7 602, 4.8 ms | 17 245, 3.9 ms |
| seeded raw listing | 15 742, 2.9 ms | 19 258, 2.6 ms |

## What was changed

The worst bottleneck was the npm packument. A package with N versions was
rendered from N version manifests on every request: N asset lookups, N blob
reads and a full JSON render. In the npm scenario the 100-version packument
had a p95 of 99 ms, and its CPU cost slowed every other request on the node.
Three changes fixed it:

- Version manifests are listed with one query and cached in memory by blob
  digest. Blobs are immutable, so the cache needs no invalidation.
- A rendered hosted packument is reused while its inputs are unchanged. The
  inputs are the versions, their creation times and quarantine flags, the
  manifest digests and the package-state digest. Any publish, dist-tag change,
  deprecation, unpublish or quarantine changes the fingerprint, so a stale
  document cannot be served.
- A packument read now costs three indexed queries and no blob read once
  warm.

Other changes:

- **Repository lookups**: every format request resolves its repository, and
  groups also resolve their members. `Get`, `GetByID` and `Members` are now
  cached for at most 30 s. Writes on this node invalidate the cache at once;
  writes on other nodes invalidate it through `repository.*` events.
- **Maven metadata**: generated `maven-metadata.xml` is cached per hosted
  repository. Entries are dropped on component and asset events from any node,
  and synchronously by this node's deploys and deletes. The document and its
  checksums come from the same bytes.
- **Query plans**: the component and asset listings used `$1 IS NULL OR ...`
  filters and `path LIKE $3`. Under a generic plan, which PostgreSQL may choose
  for a prepared statement after five executions, these cannot use the
  indexes. `EXPLAIN ANALYZE` with `plan_cache_mode = force_generic_plan` on the
  seeded data showed:

  | Query | Before | After |
  |---|---|---|
  | assets under a path prefix (one npm package, one Maven directory) in a repository of 83 330 assets | 149 ms, bitmap scan of the whole repository | 0.6 ms, range scan on `(repo_id, path COLLATE "C")` |
  | components by namespace and name | 5.0 ms, filter over 8 333 rows | 0.6 ms, unique index |
  | NuGet text search | scan of every component, computing the document | GIN index `components_document_idx` |

  Statements now contain only the filters in use. Path prefixes are turned
  into a range: `>= prefix` and `<` the prefix with its last code point
  incremented.
- **NuGet search** on hosted repositories uses a full-text index (migration
  `00070`) instead of loading every component of the repository.
- **npm dist-tags and package state** use a PostgreSQL advisory lock per
  package, which is correct across nodes. The locks first shared four
  connections of the main pool. In the mixed scenario that made every npm
  write wait for a slot: dist-tag updates averaged 830 ms. The locks now use
  a dedicated pool of up to 16 connections, and dist-tag updates average
  246 ms at a higher overall request rate.

## Known limits

- **Writes are bound by flushes to disk.** A publish writes its blobs with a
  file and directory fsync and commits one or two PostgreSQL transactions.
  An npm publish takes about 30 ms on its own. Eight publishes in parallel
  take about 140 ms each, so this machine sustains about 55 npm publishes per
  second; the previous build measured the same. On macOS, Go's `fsync` is a
  full device flush (`F_FULLFSYNC`). That is probably most of this cost, but
  it was not profiled. Linux servers with SSDs flush much faster.
- **Updates to one npm package are serialised** by its lock, cluster-wide.
  Each update takes 25-40 ms on this machine, so one package accepts at most
  about 30 dist-tag or publish operations per second. Different packages do
  not wait for each other.
- **NuGet search for a very common word is expensive.** In the seeded data
  every package is called `pkg-<n>`, so `pkg` matches all 100 000 components
  and each query costs about 15 ms of database time. Clients do not search
  during `dotnet restore`; only interactive search is affected.
- **Packument rendering for proxy and group repositories** is not cached
  (upstream documents are cached as assets). The group merge renders on every
  request.
- **Advisory locks add connections**: each node opens up to 16 lock
  connections beyond `database.max_conns`, created on demand and closed after
  one idle minute. Size PostgreSQL's `max_connections` for
  `nodes x (max_conns + 16)` plus other clients.
- **Cache staleness after a lost event**: a repository change can be served
  stale for up to 30 s, and generated Maven metadata for up to 1 minute, if a
  node misses the NATS event. Events are plain NATS publish/subscribe, so a
  node that is disconnected from NATS while the change happens misses it.

## Tuning

- `database.max_conns` (default 20): raise it when requests wait for
  connections, which shows up as rising p95 at low CPU. Keep the PostgreSQL
  connection budget above in mind.
- Put the blob store on fast storage. S3/MinIO adds network round trips to
  every download, but it does not use local fsync.
- Run several nodes behind a load balancer; sticky sessions are not needed.
  Give every node the same `HANGAR_DOCKER_TOKEN_KEY`.
