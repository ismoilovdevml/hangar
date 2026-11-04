# `maven` format

Implements the **Maven 2 repository layout** under `/repository/{repo}/`.
Protocol requirements and the acceptance checklist are in
[docs/FORMATS.md](../../../docs/FORMATS.md), section "Maven".

Specs: [repository layout](https://maven.apache.org/repository/layout.html),
[repository metadata](https://maven.apache.org/ref/3.9.6/maven-repository-metadata/repository-metadata.html),
[version order](https://maven.apache.org/pom.html#Version_Order_Specification).

## Behaviour

| Request | Hosted | Proxy | Group |
|---|---|---|---|
| `GET/HEAD` file | stored asset | cache, else remote (verified, cached) | first member that has it |
| `GET maven-metadata.xml` | generated from components/assets | cached remote doc, revalidated after `metadata_ttl` | merged across members |
| `GET <file>.md5/.sha1/.sha256/.sha512` | from `Asset.Checksums` | same | same |
| `GET <dir>/` | listing (HTML, or JSON with `Accept: application/json` / `?format=json`) | cached files | union of members |
| `PUT` | deploy | 405 | 405 |
| `DELETE` file, version dir or artifact dir | yes (`repo:<name>:delete`) | removes cached copies | 405 |

- **Paths**: `groupId-as-dirs/artifactId/version/artifactId-version[-classifier].ext`,
  timestamped snapshots `1.0-20260101.120000-3`. Dot segments and characters
  outside `[A-Za-z0-9_.+~-]` are rejected.
- **Metadata** (hosted) is generated on read: artifact level (`versions`,
  `latest`, `release`, `lastUpdated`, Maven version ordering, quarantined
  versions left out) and snapshot-version level (`snapshot` timestamp/buildNumber,
  `snapshotVersions` per classifier/extension, `localCopy` for non-unique
  snapshots). It reflects every deploy and delete, including deletes through
  the REST API and cleanup. Output is deterministic for a content state, so a
  document and its checksum files always agree. Client-uploaded artifact- and
  version-level documents are validated and dropped; group-level plugin
  documents (`<plugins>`) are merged and stored.
- **Checksums** are computed while streaming the upload (md5, sha1, sha256,
  sha512). Uploaded side files are validated against them (400 on mismatch)
  and not stored. Responses carry `X-Checksum-*` headers. `X-Checksum-*`
  request headers on upload are verified too.
- **Signatures** (`.asc`) are stored as assets of the component.
- **Redeploy** follows the hosted write policy (`allow`, `allow_once`, `deny`).
  SNAPSHOTs stay redeployable under `allow_once`: timestamped builds never
  collide; a non-unique SNAPSHOT file is replaced (needs `delete` on the repo).
- **Proxy**: freshness via `proxy.FreshnessOf` (metadata revalidated with
  `If-None-Match`/`If-Modified-Since`; artifacts immutable); remote checksum
  verified from `X-Checksum-SHA1`/`-MD5` headers or the remote `.sha1` file,
  mismatches answered 502 and not cached; stale copy served when the remote is
  down. Remote 404s are negative-cached by the core upstream. Cache fills are
  published as the reader; a read through a group fills a proxy member's cache
  on the group's `repo:<group>:read`.
- **Auth**: `401` with `WWW-Authenticate: Basic realm="Hangar"`, which Maven and
  Gradle need before they send configured credentials. Tokens work as the
  Basic password or as `Authorization: Bearer`.

## Configuration (`config.format`)

```yaml
format:
  version_policy: release   # release | snapshot | mixed (default)
  layout_policy: strict     # strict (default) | permissive: also store non-layout files
```

A release repository answers 404 for SNAPSHOT paths and rejects SNAPSHOT
uploads with 400 (and vice versa). Maven groups are read-only (`write_member`
is rejected).

## Tests

- `go test ./internal/formats/maven/` — unit tests and HTTP replays of the
  `mvn deploy` (release and SNAPSHOT), Maven resolution and Gradle
  `maven-publish` request sequences against `coretest` fakes; proxy and group
  behaviour against a fake remote.
- `TestRealGradle` runs the real `gradle` (publish, redeploy rejection,
  resolution of release, SNAPSHOT and a proxied dynamic version through a
  group) against an in-process server; skipped without gradle or with `-short`.
- `test/e2e/maven_test.go` (tag `e2e`): real `mvn deploy`, `mvn install`,
  `mvn dependency:get` and `gradle publish`/`build` against a running server
  with Maven Central behind a proxy repository.
