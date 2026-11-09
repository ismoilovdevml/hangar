# Formats — protocols and acceptance tests

Each format lives in `internal/formats/<name>/` and has an e2e test in
`test/e2e/<name>_test.go` that drives the **real client** in a container against a
running Hangar. A format is done only when its whole checklist passes for hosted,
proxy and group repositories.

## Docker / OCI (`docker`) — milestone M1
- Spec: OCI Distribution Spec — https://github.com/opencontainers/distribution-spec/blob/main/spec.md
- Token auth flow: https://distribution.github.io/distribution/spec/auth/token/
- Image/index manifests: https://github.com/opencontainers/image-spec
- Must support: `/v2/` ping + `WWW-Authenticate`, blob HEAD/GET, chunked & monolithic uploads,
  cross-repo mount, manifests by tag and digest (image manifest, index, Docker v2 schema 2),
  tag list with pagination, delete by digest, referrers API (cosign signatures, SBOM attestations),
  `_catalog` (filtered by RBAC). Proxy: Docker Hub token dance, `library/` prefix, rate-limit headers.
- Acceptance: `docker login/push/pull`, multi-arch `docker buildx --push`, `crane copy`,
  `cosign sign/verify`, `helm push oci://` (OCI charts), pull through `dockerhub-proxy`.

## npm (`npm`) — M1
- Registry API: https://github.com/npm/registry/blob/main/docs/REGISTRY-API.md
- Package metadata (packuments): https://github.com/npm/registry/blob/main/docs/responses/package-metadata.md
- Must support: packument (full + abbreviated `application/vnd.npm.install-v1+json`), tarball GET,
  `npm publish` (PUT), dist-tags, scoped packages, `npm login` / `npm whoami`, deprecate, unpublish
  (policy), audit endpoint passthrough/answer from own findings.
- Proxy: rewrite tarball URLs in packuments to Hangar; metadata TTL.
- Acceptance: `npm publish`, `npm install`, `pnpm install`, `yarn install` (berry) through group.

## Maven (`maven`) — M1
- Layout: https://maven.apache.org/repository/layout.html
- Metadata: https://maven.apache.org/ref/3.9.6/maven-repository-metadata/repository-metadata.html
- Must support: GAV paths, `maven-metadata.xml` generation (release & snapshot), checksums
  (.md5 .sha1 .sha256 .sha512), signatures (.asc), version policy release/snapshot/mixed,
  timestamped snapshots, redeploy policy.
- Acceptance: `mvn deploy`, `mvn install` via group, `gradle publish`, `gradle build` resolving through proxy.

## PyPI (`pypi`) — M2
- Simple API: https://packaging.python.org/en/latest/specifications/simple-repository-api/ (PEP 503 + JSON PEP 691)
- Upload API (legacy): https://warehouse.pypa.io/api-reference/legacy.html
- Must support: name normalisation, `data-requires-python`, hashes in links, yanked, `twine upload`.
- Acceptance: `twine upload`, `pip install`, `uv pip install`, `poetry` resolving via group.

## Go (`go`) — M2
- GOPROXY protocol: https://go.dev/ref/mod#goproxy-protocol
- Must support: `/@v/list`, `.info`, `.mod`, `.zip`, `/@latest`; proxy to proxy.golang.org; sumdb passthrough.
- Acceptance: `GOPROXY=… go mod download`, private module publish via upload API.

## Helm (`helm`) — M2
- Chart repo: https://helm.sh/docs/topics/chart_repository/ — `index.yaml` generation, `.tgz` upload.
- OCI charts are covered by the docker format.
- Acceptance: `helm repo add`, `helm install`, `helm cm-push` compatible upload.
- Supports: hosted upload (`POST api/charts`, `PUT <name>-<version>.tgz`), provenance (`.prov`), delete, ChartMuseum read API, generated and cached `index.yaml` with relative URLs.
- Proxy: upstream `index.yaml` cached for `metadata_ttl`, chart URLs rewritten through Hangar, archives cached forever and checked against the index digest.
- Group: merged `index.yaml`, first member wins per name and version; uploads go to `write_member`.

## NuGet (`nuget`) — M3
- V3 protocol: https://learn.microsoft.com/en-us/nuget/api/overview — service index, package content, registration, push.
- Acceptance: `dotnet nuget push`, `dotnet restore`.

## Cargo (`cargo`) — M3
- Sparse registry: https://doc.rust-lang.org/cargo/reference/registry-index.html#sparse-protocol
- Web API: https://doc.rust-lang.org/cargo/reference/registry-web-api.html
- Must support: `config.json` (`dl`, `api`, `auth-required`), index files at `1/`, `2/`, `3/{c}/`,
  `{ab}/{cd}/` with ETag / Last-Modified, publish framing (u32 JSON + u32 `.crate`), yank/unyank,
  download, search, owners list; raw-token and Bearer `Authorization`.
  Proxy: crates.io index (TTL) and `.crate` (forever, cksum-verified), `dl` rewritten to Hangar.
- Acceptance: `cargo publish`, `cargo build` with sparse index.

## APT (`apt`) — M3
- Debian repository format: https://wiki.debian.org/DebianRepository/Format — Release/InRelease (GPG-signed), Packages(.gz).
- Must support: `.deb` upload (control from `control.tar.{gz,xz,zst}`), pool layout, Packages/.gz/.xz per
  distribution, component and architecture (`Architecture: all` in every architecture), Release with
  MD5Sum/SHA1/SHA256/SHA512, InRelease and Release.gpg signed with the repository key (`public.key`,
  `public.gpg`). Proxy: Release files passed through unchanged, metadata TTL, by-hash and pool cached forever.
  Groups combine hosted members only (re-signed with the group key).
- Acceptance: `apt-get update && apt-get install` from hosted + proxy of deb.debian.org.

## RPM (`rpm`) — M3
- createrepo repodata: `repomd.xml`, `primary.xml.gz`, `filelists.xml.gz`, `other.xml.gz` (sha256).
- Must support: `.rpm` upload with the header parsed (dependencies, files, changelog, `rpm:header-range`),
  regenerated repodata, `repomd.xml.asc` for `repo_gpgcheck=1`, optional per-directory layout
  (`repodata_depth`). Proxy: `repomd.xml` (and signature) with a TTL, content-hashed repodata and packages
  forever. Groups combine hosted members only.
- Acceptance: `dnf install` from hosted + proxy of a Rocky Linux mirror.

## Raw (`raw`) — M1
- Plain PUT/GET/DELETE of arbitrary paths, directory listing HTML/JSON.
- Acceptance: `curl --upload-file`, `curl -O`, `wget`.
