# First administrator

Sign-in is required by default: until an administrator exists, nobody can use
the UI or the API. Create the first one in one of two ways. Both are no-ops
once an enabled administrator exists, so they are safe to leave in automation.

## From the environment (on start)

When `bootstrap.admin_user` and `bootstrap.admin_password`
(`HANGAR_BOOTSTRAP_ADMIN_USER`, `HANGAR_BOOTSTRAP_ADMIN_PASSWORD`) are both set
and no enabled administrator exists, `hangar serve` creates that local user
with the built-in `Admin` role and logs `bootstrap administrator created`.

| Install | Where |
|---|---|
| Compose (`deploy/compose`) | `HANGAR_BOOTSTRAP_ADMIN_USER` / `HANGAR_BOOTSTRAP_ADMIN_PASSWORD` in `.env` |
| Helm | `bootstrapAdmin.username` / `password`, or `bootstrapAdmin.existingSecret` (keys `admin-username`, `admin-password`) |
| systemd | `/etc/hangar/hangar.env` |

The password must satisfy `auth.password` (default 12 to 256 characters, not
containing the user name). After the first sign-in, change the password in
the UI (My account) and remove the bootstrap settings: the password would
otherwise stay readable in the environment.

## With the CLI (any time)

`hangar admin bootstrap` writes directly to the database with the server's
configuration; the server does not need to be running.

```bash
# Compose
echo 'a-long-password' | docker compose exec -T hangar \
  hangar admin bootstrap --username admin --password-stdin

# Kubernetes
kubectl -n hangar exec -i deploy/hangar -- \
  hangar admin bootstrap --username admin --password-stdin <<< 'a-long-password'

# systemd host
sudo systemd-run --uid=hangar --wait --pipe --quiet \
  -p EnvironmentFile=/etc/hangar/hangar.env -E HANGAR_CONFIG=/etc/hangar/hangar.yaml \
  /usr/local/bin/hangar admin bootstrap --username admin --password-stdin <<< 'a-long-password'
```

## First token for automation

CI systems and scripts should use tokens, not passwords. Create one in the UI
(My account, Access tokens) or, for provisioning scripts, with the CLI:

```bash
docker compose exec -T hangar hangar admin token create \
  --user admin --name provisioning --scopes 'system:*:*' --expires 30d
```

The token (`hgr_...`) is printed once. Scope it as narrowly as the job
allows; roles, service accounts and scopes are described in
[guides/access.md](../guides/access.md).

## Locked out

- Five failed passwords in 15 minutes lock an account for 15 minutes
  (`auth.lockout`); wait, or have another administrator unlock it
  (`POST /api/v1/users/{id}/unlock`).
- Lost the only administrator password: `hangar admin bootstrap` does nothing
  while an enabled administrator exists. Mint a token for that account with
  the CLI, which needs only database access, and set a new password through
  the API:

  ```bash
  TOKEN=$(docker compose exec -T hangar hangar admin token create \
    --user admin --name recovery --scopes 'system:*:*' --expires 1d)
  # find the user id, then PUT /api/v1/users/{id}/password (see api/openapi.yaml)
  curl -fsS -H "Authorization: Bearer $TOKEN" https://hangar.example.com/api/v1/users
  ```

  Revoke the recovery token afterwards.
