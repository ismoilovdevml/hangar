# `docker` format

Implements the **Docker / OCI** format: the
[OCI Distribution Specification](https://github.com/opencontainers/distribution-spec/blob/main/spec.md)
on `/v2/`, with the [token authentication flow](https://distribution.github.io/distribution/spec/auth/token/).
Protocol requirements and the acceptance checklist are in
[docs/FORMATS.md](../../../docs/FORMATS.md), section "Docker / OCI".

## Naming

OCI clients only talk to `/v2/` at the host root, so the first path segment of
an image name selects the Hangar repository:

```
docker push hangar.example.com/docker-hosted/team/app:1.0
            └── host ────────┘ └── repo ───┘ └ image ┘
```

`/v2/<repository>/<image path>/...`. An image path is required
(`hangar.example.com/docker-hosted:1.0` is rejected with `NAME_INVALID`).
Dedicated connector host names / ports (`RepoConfig.Connector`) are not routed
yet. `/repository/<repo>/...` answers 404 with a hint to use `/v2/`.

## Authentication

- `GET /v2/` answers `401` with
  `WWW-Authenticate: Bearer realm="<server.base_url>/v2/token",service="<host>"`
  followed by a `Basic realm="Hangar"` challenge, and `200` for authenticated callers.
- `GET /v2/token?service=&scope=repository:<name>:pull,push` (HTTP Basic with a
  password or a Hangar token, or anonymous) and the OAuth2 password grant
  `POST /v2/token` issue a registry token. Requested actions the caller is not
  allowed are dropped from the token (`pull`=read, `push`=write,
  `delete`=delete, `*`=all; `registry:catalog:*` for the catalog). Push to a
  group also grants the group's write member; pull grants the members.
- Registry tokens are compact JWTs signed with **HMAC-SHA256** using the key in
  **`HANGAR_DOCKER_TOKEN_KEY`** (or `Options.TokenKey`). Use at least 32 random
  bytes (`openssl rand -base64 48`), keep it secret and give every node of a
  cluster the same value. Without it a random per-process key is generated and
  a warning is logged: tokens and in-flight uploads then do not survive a
  restart or work across nodes. Tokens live 15 minutes (`Options.TokenTTL`)
  and are accepted only on `/v2/`; they are verified locally, so there is no
  revocation before expiry.
- Requests may also authenticate directly with Basic auth or a `hgr_` token.
  Missing rights answer `401` (anonymous, or a token lacking the scope:
  `error="insufficient_scope"`) or `403 DENIED`.

## Storage layout

Assets inside the Hangar repository (blobs are content-addressed in the blob
store and shared by every repository on it):

| Asset path | Kind | Component |
|---|---|---|
| `<image>/blobs/<digest>` | content | none |
| `<image>/manifests/<digest>` | content | none |
| `<image>/manifests/<tag>` | content (points at the manifest blob) | `image:tag` (namespace = image directory) |
| `<image>/referrers/<subject>/<digest>` | metadata (referrers index entry) | none |

Each tag is a component, so cleanup, quarantine and download counts work per
tag. With `allow_once`, a tag cannot be moved to a different manifest
(`409 DENIED`). Manifests referencing unknown blobs (image manifests) or
unknown child manifests (indexes) are rejected with `MANIFEST_BLOB_UNKNOWN`.
Supported manifest types: OCI image manifest and index, Docker schema 2
manifest and manifest list (schema 1 is rejected).

## Uploads

Monolithic (`POST ?digest=`), POST + PUT, and chunked (`PATCH` with or without
`Content-Range`, `416` on out-of-order chunks, `GET` status, `DELETE` cancel).
No session state is kept in memory or in the database: every chunk is a blob,
the session state is a small blob, and the upload URL carries its digest plus
an HMAC (`?_state=<hex>.<mac>`), so any node can continue a session. Clients
must follow the `Location` of each response (the spec requires it). A single
streamed chunk becomes the final blob without being copied. Sessions expire
after 24 hours (`Options.UploadTTL`); orphaned chunk/state blobs are reclaimed
by blob GC, whose grace period must exceed the upload TTL.

Cross-repository mount (`POST ?mount=<digest>&from=<name>`) links the blob
without copying when both repositories share a blob store (it is copied once
otherwise) and requires read access to `from`; otherwise a normal upload
session is started.

## Proxy and group

- **Proxy**: manifests by digest and blobs are cached forever (content
  addressed). Tags are metadata: fresh for `metadata_ttl` (via
  `proxy.FreshnessOf`), then revalidated with a `HEAD` (free on Docker Hub) and
  re-downloaded only if the digest moved; when the remote is unreachable or
  answers 5xx the stale tag is served. Blobs already in the blob store are
  linked without contacting the remote; others are streamed to the client
  while they are written to the store, and the download continues filling the
  cache if the client disconnects. Docker Hub single-segment names get the
  `library/` prefix; the token dance is done by the core upstream. Rate-limit
  headers (`RateLimit-Limit`, `RateLimit-Remaining`, `Docker-RateLimit-Source`)
  are passed through. Tag lists and referrers merge the remote's answer with
  the cache. Proxies are read-only (`405`).
- **Group**: reads walk the members in order (nested groups flattened); tag
  lists, referrers and the catalog merge members. Writes go to
  `group.write_member` (a hosted docker repository); without one the group is
  read-only.

## Other endpoints

- `GET /v2/<name>/tags/list?n=&last=` (sorted, `Link: <...>; rel="next"`).
- `GET /v2/_catalog?n=&last=` lists `<repository>/<image>` (images with at
  least one tag) for repositories the caller may read.
- `GET /v2/<name>/referrers/<digest>?artifactType=` (OCI 1.1; `OCI-Subject`
  is returned when a manifest with `subject` is pushed).
- `DELETE /v2/<name>/manifests/<digest>` deletes the manifest, every tag
  pointing at it and its referrers entry; `DELETE .../manifests/<tag>` deletes
  only the tag; `DELETE /v2/<name>/blobs/<digest>` unlinks a blob.

## Tests

- `go test ./internal/formats/docker/` — HTTP-level tests with `coretest`
  fakes and an in-memory upstream registry.
- `go test -tags ggcr ./internal/formats/docker/` — real-client tests with
  go-containerregistry (`remote.Write/Image/WriteIndex/Referrers/Delete`,
  `crane.Copy/Digest/Catalog`), including the full token flow and a Docker
  Hub-style proxy. Needs go-containerregistry's `pkg/authn` dependencies in `go.sum`.
- `test/e2e/docker_test.go` (build tag `e2e`): `docker login/push/pull`, group
  push/pull, multi-arch `docker buildx --push`, Docker Hub proxy, and
  `crane copy`, `cosign sign/verify`, `helm push oci://` when installed.
