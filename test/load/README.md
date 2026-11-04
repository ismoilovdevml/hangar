# Load tests

[k6](https://k6.io) scenarios that drive a running Hangar the way package
clients do. Results and the reference hardware are in
[docs/operations/performance.md](../../docs/operations/performance.md).

| Scenario | Traffic |
|---|---|
| `raw.js` | raw downloads of 1 KiB, 4 KiB, 64 KiB and 1 MiB files |
| `docker.js` | OCI blob GETs (4 KiB to 1 MiB) with a registry bearer token |
| `npm.js` | abbreviated and full packuments (one package with 100 versions, 20 with 3) and tarballs |
| `maven.js` | `maven-metadata.xml`, its `.sha1`, POMs and jars of 10 artifacts x 20 versions |
| `mixed.js` | 90 % reads across npm, Maven and raw, 10 % writes: npm publishes, Maven deploys, raw uploads, npm dist-tag updates |
| `seeded.js` | metadata reads (Maven metadata, raw listings, NuGet search) on repositories filled by `cmd/hangar-seed` |

Each scenario's `setup()` creates its repositories (`load-*`) and publishes its
fixtures through the normal client protocols, so it runs against an empty
server and can be re-run. Requests made by `setup()` are excluded from the
thresholds and the per-endpoint numbers (they carry no `scenario` tag).

## Running

```bash
# A server to test, e.g. a local one on :8110 with a bootstrap administrator.
export HANGAR_URL=http://127.0.0.1:8110
export HANGAR_ADMIN_PASSWORD='<admin password>'   # or HANGAR_TOKEN=hgr_...

test/load/run.sh                 # every scenario, 32 VUs x 30 s each
test/load/run.sh npm maven       # selected scenarios
HANGAR_VUS=64 HANGAR_DURATION=2m test/load/run.sh mixed
```

`run.sh` prints the per-endpoint lines and keeps each full k6 summary in
`$HANGAR_LOAD_OUT` (default `test/load/load-results/`, git-ignored). It exits
non-zero when a threshold fails: no failed requests, read p95 below 250 ms,
write p95 (mixed) below 1 s.

For `seeded.js`, fill the database first (rows only; the seeded assets have no
bytes, so they are listed and searched but not downloaded):

```bash
go run ./cmd/hangar-seed --database-url "$HANGAR_DATABASE_URL"           # 100 000 components, 1 000 000 assets
go run ./cmd/hangar-seed --database-url "$HANGAR_DATABASE_URL" --clean   # remove them again
```

Do not point these scenarios at a production server: they create
repositories and publish packages.
