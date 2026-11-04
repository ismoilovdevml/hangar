# Docker and OCI

Hangar implements the OCI Distribution API with the registry token flow. Docker,
buildx, containerd, Helm (OCI charts), cosign, crane and other OCI clients work
against it.

Placeholders:

- `hangar.example.com`: the host (and port, if any) of `server.base_url`.
- `<user>`: your username or a service account name.
- `<token>`: a personal access token (`hgr_...`).
- `$HANGAR_TOKEN`: the token in an environment variable, for the API calls.

## How images are addressed

OCI clients only talk to `/v2/` at the root of a host, so the first path segment of
the image name is the Hangar repository:

```
docker push hangar.example.com/docker-hosted/team/app:1.0
            └── host ────────┘ └── repo ───┘ └ image ┘
```

- An image path after the repository is required:
  `hangar.example.com/docker-hosted:1.0` is rejected (`NAME_INVALID`).
- The repository name must also be valid in an OCI name (lower case letters, digits
  and single dashes; no trailing dash).
- `/repository/<repo>/` URLs do not serve Docker content; use `/v2/`.
- A dedicated host name or port per repository (`config.connector`) is not
  supported yet. All Docker repositories share the `server.base_url` host.

The token realm sent to clients is `<server.base_url>/v2/token`, so clients must be
able to reach `server.base_url` itself.

## Server setting for Docker

Set `HANGAR_DOCKER_TOKEN_KEY` (`docker.token_key`) to at least 32 random bytes,
the same value on every node:

```bash
export HANGAR_DOCKER_TOKEN_KEY="$(openssl rand -base64 48)"
```

Without it Hangar generates a random key at start: `docker login` sessions and
unfinished uploads then stop working after a restart.

## Hosted repository

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "docker-hosted",
        "format": "docker",
        "type": "hosted",
        "blob_store": "default",
        "config": { "hosted": { "write_policy": "allow" } }
      }'
```

With `"write_policy": "allow_once"` a tag cannot be moved to a different manifest
(`409 DENIED`), which makes release tags immutable.

Log in, push and pull:

```bash
echo '<token>' | docker login hangar.example.com -u <user> --password-stdin

docker build -t hangar.example.com/docker-hosted/team/app:1.0 .
docker push hangar.example.com/docker-hosted/team/app:1.0
docker pull hangar.example.com/docker-hosted/team/app:1.0
```

Your account password also works for `docker login`, but a token is recommended.

## Proxy repository (Docker Hub)

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "docker-hub",
        "format": "docker",
        "type": "proxy",
        "blob_store": "default",
        "config": { "proxy": { "remote_url": "https://registry-1.docker.io", "metadata_ttl": "1h" } }
      }'
```

```bash
docker pull hangar.example.com/docker-hub/busybox:1.36          # library/ is added for one-segment names
docker pull hangar.example.com/docker-hub/library/alpine:3.20
```

- Blobs and manifests by digest are cached forever; tags are revalidated after
  `metadata_ttl`. When the remote is down, the cached tag is served.
- Docker Hub rate-limit headers are passed through. To pull with a Docker Hub
  account, add `"auth": {"kind": "basic", "username": "<hub-user>", "password": "<hub-token>"}`
  to `config.proxy`.
- Proxy repositories are read-only (`405` on push).

## Group repository

```bash
curl -X POST https://hangar.example.com/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name": "docker-group",
        "format": "docker",
        "type": "group",
        "config": { "group": { "members": ["docker-hosted", "docker-hub"], "write_member": "docker-hosted" } }
      }'
```

```bash
docker pull hangar.example.com/docker-group/team/app:1.0         # from docker-hosted
docker pull hangar.example.com/docker-group/library/alpine:3.20  # from docker-hub
docker push hangar.example.com/docker-group/team/app:1.1         # stored in docker-hosted
```

Reads try the members in order. Pushes go to `write_member`; without one the group
is read-only. Groups take no `blob_store`.

## Multi-arch builds with buildx

```bash
docker buildx create --name hangar --driver docker-container --use
docker buildx build --platform linux/amd64,linux/arm64 \
  -t hangar.example.com/docker-hosted/team/app:1.0 --push .
docker buildx imagetools inspect hangar.example.com/docker-hosted/team/app:1.0
```

The `docker-container` builder has its own registry settings. For a plain-HTTP
registry, give it a `buildkitd.toml`:

```toml
[registry."hangar.example.com:8080"]
  http = true
  insecure = true
```

```bash
docker buildx create --name hangar --driver docker-container \
  --driver-opt network=host --config buildkitd.toml --use
```

OCI image indexes and Docker manifest lists are both supported. Docker schema 1
manifests are rejected.

## Helm charts (OCI)

Helm stores charts in any Docker repository:

```bash
echo '<token>' | helm registry login hangar.example.com -u <user> --password-stdin
helm package mychart
helm push mychart-0.1.0.tgz oci://hangar.example.com/docker-hosted/charts
helm pull oci://hangar.example.com/docker-group/charts/mychart --version 0.1.0
helm install demo oci://hangar.example.com/docker-group/charts/mychart --version 0.1.0
```

Against a plain-HTTP server add `--plain-http` to `registry login`, `push`, `pull`
and `install`. Classic Helm chart repositories (`index.yaml`, `helm repo add`) are
not supported yet; use OCI.

## Signing with cosign

Signatures are stored next to the image (OCI referrers are supported):

```bash
cosign generate-key-pair
cosign login hangar.example.com -u <user> -p '<token>'
cosign sign --key cosign.key hangar.example.com/docker-hosted/team/app:1.0
cosign verify --key cosign.pub hangar.example.com/docker-hosted/team/app:1.0
```

For an isolated setup without a transparency log, add `--tlog-upload=false` to
`sign` and `--insecure-ignore-tlog=true` to `verify`. Against a plain-HTTP server
add `--allow-insecure-registry --allow-http-registry` to both.

## Plain HTTP (insecure registry)

Docker uses HTTPS for every registry except `localhost` / `127.0.0.0/8`. To use a
Hangar served over plain HTTP, list it in `/etc/docker/daemon.json` (Docker
Desktop: Settings, Docker Engine) and restart the daemon:

```json
{ "insecure-registries": ["hangar.example.com:8080"] }
```

Prefer TLS in front of Hangar for anything beyond a test setup.

## Other operations

| Task | Request |
|---|---|
| List tags | `GET /v2/<repo>/<image>/tags/list` (`?n=&last=` paging) |
| Catalog | `GET /v2/_catalog` (images you can read, as `<repo>/<image>`) |
| Delete a tag | `DELETE /v2/<repo>/<image>/manifests/<tag>` |
| Delete a manifest | `DELETE /v2/<repo>/<image>/manifests/<digest>` (also removes its tags) |

```bash
curl -u <user>:'<token>' https://hangar.example.com/v2/docker-hosted/team/app/tags/list
```

Registry tokens issued by `/v2/token` live 15 minutes and are checked locally;
revoking the personal access token stops new logins but not tokens already issued.
