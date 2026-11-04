# Getting started

From nothing to a working repository in about five minutes: run Hangar, create the
first administrator, create a token, create a hosted repository, upload and
download a file.

Placeholders used below:

- `http://localhost:8080`: the server's `server.base_url`. Replace it with your
  own URL (for example `https://hangar.example.com`) once Hangar runs elsewhere.
- `<token>`: a personal access token (starts with `hgr_`), created in step 3.
- `<admin-password>`: the administrator password you choose in step 2.

## 1. Run the server

You need PostgreSQL 16 or newer. Hangar applies its schema migrations itself.

### Option A: from source

Requires Go (see `go.mod`). Hangar embeds NATS and stores blobs on disk under
`./data` by default.

```bash
export HANGAR_DATABASE_URL='postgres://hangar:hangar@localhost:5432/hangar?sslmode=disable'
export HANGAR_BOOTSTRAP_ADMIN_USER=admin
export HANGAR_BOOTSTRAP_ADMIN_PASSWORD='<admin-password>'
go run ./cmd/hangar serve --migrate
```

The server listens on `:8080` and assumes `http://localhost:8080` is its public URL.
Change either with `HANGAR_SERVER_ADDR` and `HANGAR_SERVER_BASE_URL`. A binary built
this way has no web UI (it serves a placeholder page); the container image and
`make build-embed` include the UI.

### Option B: Docker Compose

The bundled stack runs PostgreSQL, NATS, MinIO, a Trivy server and Hangar:

```bash
docker compose -f deploy/docker-compose.yml up -d --build
curl http://localhost:8080/readyz
```

The compose file does not set a bootstrap administrator. Create one in the running
container (step 2, option B).

### Configuration

Every setting can come from a YAML file (`hangar serve --config hangar.yaml`, see
`hangar.example.yaml`) or from an environment variable named `HANGAR_` plus the
upper-cased YAML path: `server.base_url` is `HANGAR_SERVER_BASE_URL`,
`storage.fs.path` is `HANGAR_STORAGE_FS_PATH`. Environment variables win over the
file. Settings you will want early:

| Setting | Environment variable | Default |
|---|---|---|
| `server.addr` | `HANGAR_SERVER_ADDR` | `:8080` |
| `server.base_url` | `HANGAR_SERVER_BASE_URL` | `http://localhost:8080` |
| `database.url` | `HANGAR_DATABASE_URL` | (required) |
| `storage.fs.path` | `HANGAR_STORAGE_FS_PATH` | `./data/blobs` |
| `nats.data_dir` | `HANGAR_NATS_DATA_DIR` | `./data/nats` |
| `docker.token_key` | `HANGAR_DOCKER_TOKEN_KEY` | random per process |
| `bootstrap.admin_user` | `HANGAR_BOOTSTRAP_ADMIN_USER` | (none) |
| `bootstrap.admin_password` | `HANGAR_BOOTSTRAP_ADMIN_PASSWORD` | (none) |

Set `HANGAR_DOCKER_TOKEN_KEY` (32+ random bytes, e.g. `openssl rand -base64 48`)
before using Docker repositories, so registry logins survive restarts.

Check that the server is up:

```bash
curl http://localhost:8080/readyz
curl http://localhost:8080/api/v1/system/status
```

## 2. Create the first administrator

Option A: environment variables (used above). When `HANGAR_BOOTSTRAP_ADMIN_USER`
and `HANGAR_BOOTSTRAP_ADMIN_PASSWORD` are both set and no enabled administrator
exists, `hangar serve` creates that local user with the built-in `Admin` role.
Once an administrator exists, these variables change nothing. You can remove them
after the first start.

Option B: the CLI, which talks to the database directly (no running server
needed; it reads the same configuration):

```bash
echo '<admin-password>' | hangar admin bootstrap --username admin --password-stdin

# Docker Compose stack:
echo '<admin-password>' | docker compose -f deploy/docker-compose.yml exec -T hangar \
  hangar admin bootstrap --username admin --password-stdin
```

Password rules (defaults): 12 to 256 characters, must not contain the username.
Five failed sign-ins within 15 minutes lock the account for 15 minutes.

## 3. Sign in and create a personal access token

Package clients and scripts should use tokens, not your password. The REST API
accepts your username and password over HTTP Basic, so one `curl` call creates a
token:

```bash
curl -u admin:'<admin-password>' -X POST http://localhost:8080/api/v1/tokens \
  -H 'Content-Type: application/json' \
  -d '{
        "name": "laptop",
        "scopes": ["repo:*:admin", "system:repos:admin"],
        "expires_at": "2027-06-30T00:00:00Z"
      }'
```

The response contains the secret once, in `secret`:

```json
{"id":"1bf1dce4-...","name":"laptop","owner":"admin","prefix":"hgr_1Hzo",
 "scopes":["repo:*:admin","system:repos:admin"],"expires_at":"2027-06-30T00:00:00Z",
 "created_at":"...","secret":"hgr_1HzoFiZr..."}
```

- `scopes` narrow what the token may do; the token never gets more than its owner's
  roles. `repo:*:admin` plus `system:repos:admin` lets this token create and manage
  repositories. A CI token should be much narrower; see [ci.md](ci.md) and
  [access.md](access.md).
- `expires_at` is required, RFC 3339, in the future and at most 366 days away
  (`auth.max_token_lifetime`).

Alternatively, create a token from the command line on the server host:

```bash
hangar admin token create --user admin --name laptop \
  --scopes 'repo:*:admin,system:repos:admin' --expires 30d
```

Keep the token in an environment variable for the rest of this guide:

```bash
export HANGAR_TOKEN='<token>'
```

Browser sign-in: the web UI signs in with `POST /api/v1/auth/login` and a session
cookie. The cookie is marked `Secure`, which browsers accept on `https://` and on
`http://localhost`. To use the UI over plain HTTP on another host name, set
`HANGAR_AUTH_SESSION_INSECURE=true` (development only).

## 4. Create a hosted repository

```bash
curl -X POST http://localhost:8080/api/v1/repositories \
  -H "Authorization: Bearer $HANGAR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
        "name": "raw-hosted",
        "format": "raw",
        "type": "hosted",
        "blob_store": "default",
        "config": { "hosted": { "write_policy": "allow" } }
      }'
```

The answer is `201 Created` with the repository, including the URL clients use:

```json
{"id":"...","name":"raw-hosted","format":"raw","type":"hosted","blob_store":"default",
 "online":true,"config":{"hosted":{"write_policy":"allow"},"anonymous_read":false,...},
 "url":"http://localhost:8080/repository/raw-hosted/","status":"online",...}
```

Repository fields:

| Field | Notes |
|---|---|
| `name` | `^[a-z0-9][a-z0-9-]{1,62}$`, immutable |
| `format` | `docker`, `maven`, `npm`, `nuget`, `pypi` or `raw`, immutable |
| `type` | `hosted`, `proxy` or `group`, immutable |
| `blob_store` | required for hosted and proxy (the store created on first start is `default`); must be absent for groups |
| `online` | default `true`; offline repositories answer 503 |
| `config.hosted.write_policy` | `allow` (default), `allow_once` (a path can be written once; identical re-uploads are accepted) or `deny` |
| `config.proxy` | `remote_url` (required), `metadata_ttl`, `content_ttl`, `negative_cache_ttl`, `auth` |
| `config.group` | `members` (ordered, required), `write_member` (formats that support group writes) |
| `config.anonymous_read` | `true` lets unauthenticated clients read |
| `config.format` | format-specific settings, described in each client guide |

The API also accepts the same document as YAML (`Content-Type: application/yaml`).

## 5. Upload and download a file

```bash
echo 'hello from hangar' > hello.txt

# upload (201 Created; 200 when a file is replaced)
curl -u "admin:$HANGAR_TOKEN" --upload-file hello.txt \
  http://localhost:8080/repository/raw-hosted/demo/hello.txt

# download
curl -u "admin:$HANGAR_TOKEN" -O http://localhost:8080/repository/raw-hosted/demo/hello.txt
```

With a token as the password, the username is not checked; any value works.

## Next steps

- Connect a package client: [Docker](docker.md), [npm](npm.md), [Maven](maven.md),
  [PyPI](pypi.md), [NuGet](nuget.md), [raw files](raw.md).
- Publish from pipelines with a service account: [CI integration](ci.md).
- Automate with the [REST API](api.md).
- Users, roles and scopes: [access.md](access.md).
