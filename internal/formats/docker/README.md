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
`/repository/<repo>/...` answers 404 with a hint to use `/v2/`.

## Connectors

A connector gives one docker repository an entry point of its own, where
image names carry no repository prefix (`cr.example.com/team/app:1.0`).
Configure it on the repository (`config.connector`):

| Setting | Effect |
|---|---|
| `host: cr.example.com` | Requests whose effective `Host` is that name (case-insensitive, any port; `X-Forwarded-Host` only from `server.trusted_proxies`) are served by this repository. |
| `http_port: 5001` | Hangar opens a plain-HTTP listener on that port (same interface as `server.addr`) serving only this repository's `/v2/` API and `/healthz`; the UI and REST API are not exposed there. Listeners open and close as repositories change; a port in use is logged and retried. |

- A host or port can belong to one repository only, and the host cannot be
  the `server.base_url` host (validated on create/update).
- Registry tokens follow the entry point: the `WWW-Authenticate` challenge
  of a host connector is
  `Bearer realm="<base_url scheme>://cr.example.com/v2/token",service="cr.example.com"`,
  so clients that can only reach `cr.example.com` get their tokens there.
  A port connector advertises the token endpoint on the address the client
  used for the port (`http://<host>:<port>/v2/token`, or `https` when a
  trusted proxy sends `X-Forwarded-Proto`) with
  `service="<base_url host>:<port>"`; the main token endpoint also accepts
  that service. Scopes name images without the prefix
  (`repository:team/app:pull,push`), and a token is accepted only on the
  entry point it was issued for.
- `docker login cr.example.com`, `helm registry login cr.example.com` and
  CI credentials work unchanged; authorization is that of the repository
  behind the connector. The catalog on a connector lists only its images.

Reverse proxy for a host connector (TLS terminated in front; Hangar must
trust the proxy in `server.trusted_proxies`, and the proxy must pass the
original `Host` or `X-Forwarded-Host`):

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name cr.example.com;
    ssl_certificate     /etc/ssl/cr.example.com.crt;
    ssl_certificate_key /etc/ssl/cr.example.com.key;

    client_max_body_size 0;          # layers can be many GiB
    proxy_request_buffering off;     # stream uploads
    proxy_buffering off;             # stream downloads
    proxy_read_timeout 900s;
    proxy_send_timeout 900s;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-Host  $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    }
}
```

```caddyfile
cr.example.com {
    reverse_proxy 127.0.0.1:8080 {
        flush_interval -1
        transport http {
            read_timeout 15m
            write_timeout 15m
        }
    }
}
```

Caddy forwards the original `Host` and sets `X-Forwarded-Host`/`-Proto`
itself. For a port connector, point the proxy (or a load balancer
listener) at `127.0.0.1:<http_port>` instead; the `Host` header is then
irrelevant.

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
No session state is kept in memory or in the database, so any node can
continue a session and sessions survive restarts (all nodes need the same
`HANGAR_DOCKER_TOKEN_KEY` and blob store):

- Every chunk is streamed straight into the blob store as a
  content-addressed blob. A single streamed chunk (docker, containerd,
  go-containerregistry, podman) becomes the final blob without being copied;
  several chunks are concatenated on `PUT`.
- The session record lives in the blob store's temp area
  (`core.TempStore`: `<root>/state/docker/uploads/<uuid>` on the filesystem
  driver, `<prefix>state/docker/uploads/<uuid>` on S3), outside the blob tree.
  The upload URL carries an HMAC over the session identity
  (`?_state=s.<mac>`), so a URL cannot be forged or reused for another image
  or repository. Clients must follow the `Location` of each response.
- A finished (`PUT`) or cancelled (`DELETE`) session is removed; chunks that
  did not become the blob are journaled under `docker/garbage/<uuid>`.
  Sessions expire 24 hours after they start (`Options.UploadTTL`).
- The built-in task `docker-upload-cleanup` (type `docker_upload_cleanup`,
  hourly at minute 40, `internal/tasks/dockeruploads`) calls
  `(*docker.Format).CleanupUploads(ctx, docker.UploadCleanup{Release: ...})`:
  it ends abandoned sessions and passes
  every journaled chunk to `Release`, which must register the blob as
  unreferenced (a `blobs` row with `ref_count 0` unless one exists) so blob
  GC deletes it after its grace period unless an asset references it by
  then. Without `Release` it only counts them. Safe to run on several nodes.
- Blob stores without a temp area fall back to storing the record as a
  small blob addressed by the URL (`?_state=<hex>.<mac>`); those records
  cannot be enumerated.

Cross-repository mount (`POST ?mount=<digest>&from=<name>`) links the blob
without copying when both repositories share a blob store (it is copied once
otherwise) and requires read access to `from`; otherwise a normal upload
session is started.

## Proxy and group

- **Proxy**: manifests by digest and blobs are cached forever (content
  addressed). Tags are metadata: fresh for `metadata_ttl` (via
  `proxy.FreshnessOf`), then revalidated with a `HEAD` (free on Docker Hub) and
  re-downloaded only if the digest moved; when the remote is unreachable or
  answers 5xx the stale tag is served. Blobs already in the blob store
  (pulled through another repository or image) are linked after a `HEAD`
  confirms the remote serves them under this name, so a proxy never hands
  out stored bytes by digest alone; others are streamed to the client
  while they are written to the store, and the download continues filling the
  cache if the client disconnects. Docker Hub single-segment names get the
  `library/` prefix; the token dance is done by the core upstream. Rate-limit
  headers (`RateLimit-Limit`, `RateLimit-Remaining`, `Docker-RateLimit-Source`)
  are passed through. Tag lists (following the remote's `Link: rel="next"`
  pages) and referrers merge the remote's answer with the cache. Cache fills are published as the reader; a pull through a group
  fills a proxy member's cache on the group's `repo:<group>:read`. Proxies are
  read-only (`405`).
- **Push through a group**: blob and manifest probes on a group answer from
  any member, so a client may skip uploading a layer another member holds.
  A manifest PUT then links what the write member lacks from the member that
  serves it (same members, routing and quarantine checks as reads, fetching
  from a proxy's remote when needed; adopt.go), so the write member holds the
  whole image.
- **Remote answers for unknown names**: Docker Hub answers `401 UNAUTHORIZED`
  for repositories it does not know (they might be private), GHCR/Quay
  `403 DENIED`, some registries `400 NAME_INVALID`. All of these, and `404`,
  mean the remote does not serve the name: the proxy answers
  `MANIFEST_UNKNOWN` / `BLOB_UNKNOWN` (404) and remembers the name for
  `negative_cache_ttl` (30 s when unset), so the many probes of one push do
  not each reach the remote. With proxy credentials configured a `401/403`
  is also logged as a warning (the credentials may be wrong). While the
  remote rate limits (`core.ErrRateLimited`), cached tags, manifests and
  blobs are still served; uncached content is answered `429 TOOMANYREQUESTS`
  with the remote's `Retry-After`. Other failures are `502 UNAVAILABLE`.
- **Group**: reads walk the members in order (nested groups flattened); the
  first member holding the content decides (including its quarantine). Tag
  lists, referrers and the catalog merge members. An unreachable or
  rate-limiting remote of a proxy member counts as a miss, so the group
  answers `404` when no member has the content: pushes through a group keep
  working while Docker Hub is down, because clients probe
  `HEAD manifests/<tag>` and `HEAD blobs/<digest>` before uploading and abort
  on anything but 200/404. Writes go to `group.write_member` (a hosted docker
  repository); without one the group is read-only.

## Quarantine

Each tag is a component; a pull by tag checks that tag's `quarantined` flag.
Content reached without a tag follows one rule: **content of an image is
blocked when a quarantined tag reaches it and no clean tag does.** A tag
reaches its manifest and, for an index, the child (platform) manifests; a
blob is blocked when every manifest of the image that references it is
blocked. This covers pulls by digest (containerd resolves every tag to a
digest; platform manifests are always pulled by digest), blobs (also
manifests read through the blobs endpoint) and cross-repository mounts
(quarantined content is not mounted; the client uploads instead). Blobs
shared with a clean image and untagged manifests stay servable. Denials are
`403 DENIED`. Manifests record their references in their asset attributes
(`blobs`, `manifests`); manifests stored before that are read from the blob
store. The computed state is cached per image for 15 seconds and dropped on
every `component.*` event of the repository, so a quarantine or release
applies immediately on all nodes that receive the event.

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
  fakes and an in-memory upstream registry, plus real-client tests with
  go-containerregistry (push/pull, indexes, referrers, platform selection,
  connector host). `TestLargeLayerStreaming` streams a 5 GiB + 3 byte layer
  from a generated reader (about 10 s; skipped with `-short` and `-race`).
- `test/e2e/oci_test.go`, `test/e2e/oci_robust_test.go` (build tag `e2e`,
  no Docker daemon needed):
  `HANGAR_E2E_URL=http://127.0.0.1:8080 HANGAR_E2E_USER=... HANGAR_E2E_PASSWORD=... go test -tags e2e ./test/e2e -run OCI`
  — push/pull, push via a group with a Docker Hub proxy member, multi-arch,
  Docker Hub proxy, helm `oci://` (`--plain-http`), chunked uploads,
  concurrent pushes, connector port and host.
- `test/e2e/docker_test.go` (build tag `e2e`): `docker login/push/pull`, group
  push/pull, multi-arch `docker buildx --push`, Docker Hub proxy, and
  `crane copy`, `cosign sign/verify`, `helm push oci://` when installed.
