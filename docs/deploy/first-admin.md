# First administrator

Sign-in is required by default: until an administrator exists, nobody can use
the UI or the API. The first one is created in one of three ways. All are
no-ops once users exist (generated) or an enabled administrator exists
(environment, CLI), so they are safe to leave in automation.

Whichever way it is created, the first administrator must set a new password
at the first sign-in (see [Required password change](#required-password-change)),
unless `bootstrap.require_password_change` is `false`.

## Generated on the first start (default)

When neither `bootstrap.admin_user` nor `bootstrap.admin_password` is set and
the database has no users at all, the first `hangar serve` creates the local
user `admin` with the built-in `Admin` role and a one-time password:

- If the file `bootstrap.initial_password_file`
  (`HANGAR_BOOTSTRAP_INITIAL_PASSWORD_FILE`) exists and holds a password, that
  password is used (it must satisfy `auth.password`). Installers use this to
  choose the password themselves.
- Otherwise Hangar generates a 24-character password (letters and digits,
  without look-alike characters), prints it **once** to standard error in a
  framed `WARN` block, and, when `bootstrap.initial_password_file` is set,
  writes it to that file with mode `0600` (the directory is created with mode
  `0700` if missing). The file is written before the account is committed, so
  a start that fails half-way reuses the same password next time.

```text
WARN ========================================================================
WARN  Hangar created the first administrator.
WARN
WARN    username: admin
WARN    password: <24 characters>
WARN
WARN  This password is shown once. Sign in and set a new password: until then
WARN  the account can do nothing else.
WARN  It is also stored in /var/lib/hangar/secrets/initial-admin-password (deleted after the change).
WARN ========================================================================
```

The block is written straight to standard error, never through the
structured logger: it is not kept in the in-memory buffer behind
**System logs**, and the structured log only notes that the administrator was
created and where the password went. Read it with `docker compose logs
hangar`, `kubectl logs` or `journalctl -u hangar`, or from the file.

The file is deleted automatically once the administrator sets a new password.
With several nodes starting at once, exactly one creates the user (a
database advisory lock); only that node prints the block and writes its
file.

## From the environment (on start)

When `bootstrap.admin_user` and `bootstrap.admin_password`
(`HANGAR_BOOTSTRAP_ADMIN_USER`, `HANGAR_BOOTSTRAP_ADMIN_PASSWORD`) are both set
and no enabled administrator exists, `hangar serve` creates that local user
with the built-in `Admin` role and logs `bootstrap administrator created`. It
must set a new password at the first sign-in unless
`bootstrap.require_password_change` (`HANGAR_BOOTSTRAP_REQUIRE_PASSWORD_CHANGE`)
is `false`, for automated setups that keep using the configured password.

| Install | Where |
|---|---|
| Compose (`install.sh`) | Generated: `admin` with the password in `<data>/secrets/initial-admin-password` (`bootstrap.admin_password_file`), printed once by the installer; the file is removed after the first password change |
| Helm | `bootstrapAdmin.username` / `password`, or `bootstrapAdmin.existingSecret` (keys `admin-username`, `admin-password`) |
| systemd | `/etc/hangar/hangar.env` |

The password must satisfy `auth.password` (default 12 to 256 characters, not
containing the user name). After the first sign-in, change the password in
the UI (My account) and remove the bootstrap settings: the password would
otherwise stay readable in the environment.

## With the CLI (any time)

`hangar admin bootstrap` writes directly to the database with the server's
configuration; the server does not need to be running. The administrator it
creates must set a new password at the first sign-in, unless
`bootstrap.require_password_change` is `false`.

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

## Required password change

A local user who must change their password and signs in with it (in the UI
or with HTTP Basic) gets a limited session: only `GET /api/v1/auth/session`
(which reports `password_change_required: true`), `PUT /api/v1/me/password`
and `POST /api/v1/auth/logout` answer. Every other API call answers `403` with
code `password_change_required`; package clients using the password are
refused; no tokens can be created for the account. The UI shows only the
**Set a new password** page, with a sign-out button.

The new password must follow `auth.password` and differ from the one given.
After the change the session is replaced by a new one, the account is no
longer limited, the change is audited as `user.password.changed` with reason
`first_login`, and the initial password file is deleted if it held the old
password. Users who sign in with single sign-on (OIDC, LDAP) are never
affected.

The same applies after `hangar admin reset-password` and when an
administrator chooses **Require password change** for a member (Access, open
the member; `POST /api/v1/users/{id}/require-password-change`, audited as
`user.password.require_change`). Existing access tokens of such a user keep
working.

## First token for automation

CI systems and scripts should use tokens, not passwords. Create one in the UI
(My account, Access tokens) or, for provisioning scripts, with the CLI:

```bash
docker compose exec -T hangar hangar admin token create \
  --user admin --name provisioning --scopes 'system:*:*' --expires 30d
```

This fails while the account must still change its password: sign in once
and set a new one first, or bootstrap with
`HANGAR_BOOTSTRAP_REQUIRE_PASSWORD_CHANGE=false` in fully automated setups.

The token (`hgr_...`) is printed once. Scope it as narrowly as the job
allows; roles, service accounts and scopes are described in
[guides/access.md](../guides/access.md).

## Locked out

- Five failed passwords in 15 minutes lock an account for 15 minutes
  (`auth.lockout`); wait, or have another administrator unlock it
  (`POST /api/v1/users/{id}/unlock`).
- Lost the only administrator password: `hangar admin bootstrap` does nothing
  while an enabled administrator exists. Give the account a one-time password
  with the CLI, which needs only database access:

  ```bash
  docker compose exec -T hangar hangar admin reset-password --user admin
  ```

  It prints a new password once (on standard output), clears any lockout,
  ends the account's sessions and makes it set its own password at the next
  sign-in. A disabled account stays disabled. The reset is audited as
  `user.password` by `system` with `reset_by: cli`.
