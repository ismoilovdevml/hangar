# `nuget` format

Implements the **NuGet V3** protocol for hosted, proxy and group repositories.
Protocol requirements and the acceptance checklist are in
[docs/FORMATS.md](../../../docs/FORMATS.md), section "NuGet". Specs:
<https://learn.microsoft.com/en-us/nuget/api/overview>.

Client source URL: `https://<hangar>/repository/<repo>/index.json`.

## Endpoints (relative to `/repository/{repo}/`)

| Method | Path | Resource |
|---|---|---|
| GET/HEAD | `index.json` | service index (served without authentication) |
| GET/HEAD | `v3-flatcontainer/{id}/index.json` | `PackageBaseAddress/3.0.0` version list (listed + unlisted) |
| GET/HEAD | `v3-flatcontainer/{id}/{ver}/{id}.{ver}.nupkg` | package content (Range, ETag) |
| GET/HEAD | `v3-flatcontainer/{id}/{ver}/{id}.nuspec` | manifest |
| GET/HEAD | `v3/registration/{id}/index.json` | `RegistrationsBaseUrl` (SemVer 1 only) |
| GET/HEAD | `v3/registration-gz/...` | `RegistrationsBaseUrl/3.4.0` (gzip, SemVer 1 only) |
| GET/HEAD | `v3/registration-gz-semver2/...` | `RegistrationsBaseUrl/3.6.0` (gzip, all versions) |
| GET/HEAD | `v3/{hive}/{id}/page/{lower}/{upper}.json`, `v3/{hive}/{id}/{ver}.json` | registration pages and leaves |
| GET/HEAD | `query` | `SearchQueryService` (`q`, `skip`, `take`, `prerelease`, `semVerLevel`, `packageType`) |
| PUT | `api/v2/package` | `PackagePublish/2.0.0` push (multipart, `X-NuGet-ApiKey`) |
| DELETE | `api/v2/package/{id}/{ver}` | unlist (or delete, see config) |
| POST | `api/v2/package/{id}/{ver}` | relist |
| PUT | `api/v2/symbolpackage` | `SymbolPackagePublish/4.9.0` (.snupkg stored next to the package) |

Registration paging follows nuget.org: below 128 versions all pages (64
versions each) are inlined in the index, from 128 on pages are linked.

## Model

- One component per package version: namespace `""`, name = lowercased id,
  version = lowercased normalized version without build metadata.
  Attributes: `id` (original case), `version` (normalized, with metadata),
  `listed`, `semver2`, `sha512`, `nuspec` (manifest metadata, catalog-entry
  shaped), `description`, `authors`.
- Assets at their flat container paths: `.nupkg` (content, `sha512`
  checksum), `.nuspec` (content, extracted on push), optional `.snupkg`.
- Proxy metadata cache: `_upstream/index.json`, `_upstream/flat/{id}/index.json`,
  `_upstream/registration/{id}.json` (assembled catalog entries), kind metadata.

## Behaviour

- Auth: `X-NuGet-ApiKey` (a Hangar token), else Basic (token as password) or
  Bearer. 401 answers carry `WWW-Authenticate: Basic`, so `dotnet restore`
  uses `packageSourceCredentials`. Push needs `repo:<name>:write`, unlist
  `delete` (and `write`, it updates the component), relist `write`.
- Push streams the first multipart part into the blob store while hashing
  (SHA-512), then reads only the zip central directory and the `.nuspec`
  entry from the stored blob. Write policy is the repository's: `allow_once`
  accepts identical bytes and answers 409 for a different package of the same
  id/version.
- Unlisted versions are hidden from search and marked `listed: false`
  (published 1900-01-01) in registrations; they stay in the flat container and
  downloadable.
- Proxy: `remote_url` is the feed base (`https://api.nuget.org`); the
  service index path is `format.service_index` (default `v3/index.json`).
  Metadata is cached for `metadata_ttl` (0 = 1h, negative = always
  revalidate), stale copies are served when the remote fails; packages are
  cached forever. Every upstream URL is rewritten to Hangar. Resources outside
  `remote_url` (nuget.org's search host) are fetched with `Deps.HTTPClients`
  without the remote's credentials. Cache fills are published as the reader;
  a read through a group fills a proxy member's cache on the group's
  `repo:<group>:read`.
- Group: versions, registrations and search results are merged across
  members (first member wins per version); content comes from the first member
  that has it; pushes go to `write_member`.

## Config (`config.format`)

| Key | Values | Default |
|---|---|---|
| `delete_mode` | `unlist`, `delete` | `unlist` |
| `service_index` | path relative to `remote_url` (proxy) | `v3/index.json` |

## Tests

- `go test ./internal/formats/nuget/` — unit tests on coretest fakes plus
  `TestDotnetClient`, which drives the real `dotnet` CLI (pack, push, add
  package, restore, build, package search, delete) against two in-process
  servers (one acts as the proxy's remote). Skipped when `dotnet` is missing.
- `HANGAR_TEST_NUGET_ORG=1` additionally restores real packages from
  api.nuget.org through a proxy.
- `test/e2e/nuget_test.go` (tag `e2e`): the same flow against a running Hangar.
