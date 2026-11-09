# Move to Hangar

Moving a team from another package registry takes four steps:

1. **Import** the packages you published yourself into hosted repositories.
2. **Pre-warm** the proxy repositories with what your builds download, so the
   first day's builds do not all go to the internet at once.
3. **Switch** the clients to Hangar.
4. **Verify** that builds resolve everything from Hangar.

Steps 1 and 2 are two task types of the task runtime (**Scheduled tasks**):
`repository_import` and `proxy_prewarm`. They read the source with the
format's own client protocol, so they work with any registry that speaks it:
another self-hosted registry, the package registries of GitLab or GitHub, or
a public registry. They write through Hangar's own client protocols, so every
imported version goes through the same checks, write policy, metadata,
scanning, audit and events as a client upload.

Runs show progress and a log on the task page, can be stopped, and resume
from a checkpoint after a restart or a node failure (another node takes the
run over). Running a task again copies only what is missing.

## Before you start

- Create the hosted, proxy and group repositories first (see the format
  guides). An import goes into a **hosted** repository; pre-warming fills a
  **proxy** or a **group** (whose proxy members fill their caches).
- Creating or running these tasks needs `system:tasks:write` **and**
  `repo:<repository>:write` on the target repository.
- Source credentials are sealed with `security.secret_key` like proxy
  credentials; without a key, tasks with credentials are refused (`501`).
  Responses never return them; leave them empty when you edit a task to keep
  the stored ones.
- The source URL must pass the same outbound address policy as proxy remotes.
  A registry on a private network needs `http.allow_private_networks`
  (see [docs/security.md](../security.md)). The server-wide outbound proxy and
  CA settings apply.

## 1. Import hosted packages

**Scheduled tasks → New task → Import packages** (or **Import** on a hosted
repository's page). Give a name, the hosted repository, the source URL,
optional credentials and filters, then **Create and run**.

| Format | Source URL | Lists packages with | Notes |
|---|---|---|---|
| npm | registry URL (`https://old.example.com/repository/npm/`) | `/-/all`, else `/-/v1/search` | full packument per package; dist-tags, deprecations and publish times carried over |
| NuGet | V3 service index (`.../index.json`) or a V2 feed | V3 search service | registration pages give dates, hashes and listing state; unlisted versions stay unlisted; V2 `FindPackagesById()` when there is no V3 index |
| Maven | repository root | HTML directory listings (crawled from the groups your patterns name) | names are `groupId:artifactId`; without listings, name artifacts exactly and files come from the POM (`.pom`, main artifact, `-sources`, `-javadoc`, `.module`, `.asc`); timestamped snapshots from the version metadata |
| PyPI | simple index (`.../simple/`) | the simple index root (PEP 503 or 691) | every file of a version with its sha256; core metadata from the wheel or sdist; yanked releases stay yanked |
| Go | a GOPROXY | — (the protocol has no listing) | name modules exactly; `.info` keeps the source's `Time` |
| Cargo | sparse index (`sparse+https://...`) | the index's web API search, when `config.json` names one | publish metadata from the index entry and the crate's `Cargo.toml`; yanked versions stay yanked |
| Helm | chart repository URL | `index.yaml` | provenance files copied when the source has them |
| Docker | registry root (`https://registry.example.com`) | `/v2/_catalog` | every platform of a multi-arch image, blobs first; token authentication answered with the source credentials |
| raw | base URL | HTML directory listings | list paths (and directories ending in `/`) under **More options → Files** |
| APT | archive root (with `dists/` and `pool/`) | `Packages` of the suite, components and architectures | suite, components and architectures under **More options** |
| RPM | repository directory (holding `repodata/`) | primary metadata | packages keep their location |

Filters:

- **Packages**: exact names, or patterns with `*` and `?` (`@acme/*`,
  `com.acme.*:*`, `team/*`). An empty list means everything the source lists.
- **Versions**: a range (`>=1.2 <2`, `^1.4`, `~1.2`, `~=2.1`, joined with
  `||`) or glob patterns (`1.*`, `v2.*`, handy for Docker tags).
- **Newest per package**: keep only the newest N versions.
- **Pre-releases**: off by default.
- **More options**: exclusions, requests per second to the source (default
  10) and how many packages are copied at once (default 4).

What happens with each version:

- not in the repository: copied (`imported`), with the source's publish date
  where the source has one;
- present with identical content: `skipped`;
- present with other content: a `conflict`, reported on the run and left as
  it is.

A run that had failures ends as failed with the list of what failed; run it
again to retry them.

Through the API:

```bash
curl -X POST https://hangar.example.com/api/v1/tasks \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' -d '{
    "name": "import-npm",
    "type": "repository_import",
    "config": {
      "repository": "npm-hosted",
      "source": { "url": "https://old.example.com/repository/npm-hosted/", "username": "mover", "password": "..." },
      "packages": ["@acme/*"],
      "latest": 20
    }
  }'
curl -X POST -H "Authorization: Bearer $HANGAR_TOKEN" https://hangar.example.com/api/v1/tasks/import-npm/run
```

The result of a run (`GET /api/v1/tasks/import-npm/runs/{run_id}`, field
`result`) counts `imported`, `skipped`, `conflicts`, `failed` and `bytes`,
and lists the first 100 conflicts and failures.

## 2. Pre-warm proxies

**Scheduled tasks → New task → Pre-warm a proxy** (or **Pre-warm** on a proxy
or group page). Each coordinate is fetched through the repository exactly as
a client would fetch it, so it lands in the cache.

Give the coordinates in any of these ways:

- **Paste** them, one per line: `name@version` (npm, NuGet, Cargo, Helm, Go),
  `name==version` (PyPI), `groupId:artifactId[:extension[:classifier]]:version`
  (Maven), `image:tag` or `image@sha256:...` (Docker, every platform), a path
  (raw, APT, RPM).
- **Add from lockfile**: `package-lock.json`, `npm-shrinkwrap.json`,
  `pnpm-lock.yaml`, `yarn.lock` (classic and berry), `packages.lock.json`,
  `requirements.txt` (pinned `==` lines), `uv.lock`, `poetry.lock`, `go.sum`,
  `Cargo.lock`, `gradle.lockfile`, `Chart.lock`, or any plain list. The file is
  read when you add it (`POST /api/v1/lockfiles`, sent gzip-compressed) and only
  the coordinates are stored. Workspace, path, git and link dependencies are
  counted as skipped.
- **Also fetch everything a source registry holds**: give the old registry's
  proxy (or any registry) and the same filters as an import; the task lists
  what it holds and fetches all of it. This is how to cache everything the old
  registry had cached.

The run counts `cached`, `failed` and `skipped`. Names a
[routing rule](routing-rules.md) of the repository keeps from its remote are
not requested and count as skipped, with the rule, rather than failed.

## 3. Switch the clients

Point each client at the Hangar group of its format (see the format guides):
`.npmrc` `registry=`, `nuget.config`, Maven `settings.xml` mirror,
`pip.conf` `index-url`, `GOPROXY`, `.cargo/config.toml`, `helm repo add`,
Docker image names, APT `.sources` and RPM `.repo` files. CI systems take the
same settings with a service account token ([CI integration](ci.md)).

## 4. Verify

- Clean builds (empty local caches) succeed against Hangar.
- **Repositories**: the hosted repositories list the imported versions; the
  proxies' cache sizes grew.
- The import and pre-warm runs ended with no failures and no unexpected
  conflicts.
- **Overview** shows cache hits rising on the proxies.
- Keep the old registry read-only for a while; an import task can run again
  (or on a schedule) to pick up anything published there late.
