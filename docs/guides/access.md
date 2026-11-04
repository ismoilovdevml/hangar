# Access management

How people, service accounts, roles, tokens and sessions work in Hangar, and
how to operate them from the web UI (**Access & audit**, **My account**) and
the REST API (`api/openapi.yaml`, tag `access`).

## Model

| Concept | What it is |
|---|---|
| Member | A person. Local members sign in with a password; members from OIDC, LDAP or SAML are created at first sign-in and their profile is managed by the provider. |
| Service account | A non-human identity for CI and automation. It has no password and authenticates only with tokens that an administrator creates. |
| Role | A named set of privileges. Built-in: **Admin**, **Publisher**, **Reader**, **CI deployer**. Custom roles can be created, edited and deleted. |
| Privilege | `repo:<pattern>:<action>` or `system:<area>:<action>`; `system:*` is everything administrative. |
| Personal access token | A secret (`hgr_…`) that acts for its owner, limited to its **scopes** and always expiring. |
| Session | A browser sign-in (cookie). Listed with address, browser and last activity; can be ended. |

**Actions**: `read` (download, browse), `write` (publish), `delete`, `admin`
(all of these plus repository settings). `admin` and `*` imply the others.

**Patterns** are globs over repository names: `*` any characters, `?` one
character, `[ab]` a set. `npm-*` matches `npm-internal` and `npm-public`.

**System areas**: `users` (members, service accounts, roles, sessions),
`tokens` (other principals' tokens), `repos`, `blobstores`, `tasks`,
`cleanup`, `security`, `audit`, `cluster`, `settings`.

A request is allowed when one of the principal's role privileges allows it
**and**, for a token, one of the token's scopes allows it too. A token can
therefore never do more than its owner. Repositories with **anonymous read**
can be read by everyone, signed in or not.

### Built-in roles

| Role | Privileges |
|---|---|
| Admin | `repo:*:admin`, `system:*` |
| Publisher | `repo:*:read`, `repo:*:write` |
| Reader | `repo:*:read` |
| CI deployer | `repo:*:read`, `repo:*:write`, `repo:*:delete` |

Built-in roles cannot be changed or deleted. At least one enabled member
always keeps **Admin**: the last one cannot be disabled, deleted or demoted.

## Members

*Access & audit → Members*. Filter by name or email, role and status
(`active`, `locked`, `disabled`). Selecting a member opens a drawer with
their roles, tokens, sessions and effective permissions.

| Task | UI | API |
|---|---|---|
| Add a local member | **Add member** | `POST /users` |
| Edit name, email, roles | Drawer → **Edit** | `PUT /users/{id}` |
| Disable / enable | Drawer → **Disable** / **Enable** | `POST /users/{id}/disable`, `/enable` |
| Unlock after failed sign-ins | Drawer → **Unlock** | `POST /users/{id}/unlock` |
| Set a new password | Drawer → **Reset password** | `PUT /users/{id}/password` |
| Sign someone out everywhere | Drawer → **Sign out everywhere** | `DELETE /users/{id}/sessions` |
| Delete | Drawer → **Delete** (type the username) | `DELETE /users/{id}` |

- **Disabling** ends the member's sessions at once; their tokens stop working
  while the account is disabled and work again after enabling. Nothing is
  deleted. Prefer it to deleting when someone leaves.
- **Deleting** removes the account with its tokens, sessions and role
  bindings. The audit log keeps the history.
- **Reset password** clears any lockout and ends all of the member's
  sessions. Administrators change their *own* password under My account,
  which requires the current password.
- You cannot disable, delete or reset the password of your own account.

Required privileges: `system:users:read` to list and view, `system:users:write`
to change, `system:users:delete` to delete.

## Service accounts

*Access & audit → Service accounts → New service account*. Give it a name, a
note about what uses it ("GitLab runners, group infra") and roles — usually
**CI deployer**, or a custom role limited to the repositories the pipeline
publishes to. Then open it and create a token under **Tokens → New token**
(needs `system:tokens:write`). Use one token per pipeline so each can be
revoked on its own.

```bash
# GitLab CI / GitHub Actions: store the token as a masked secret
echo "$HANGAR_TOKEN" | docker login registry.example.com -u ci --password-stdin
npm config set //registry.example.com/repository/npm-internal/:_authToken "$HANGAR_TOKEN"
```

## Personal access tokens

*My account → Personal access tokens → New token*, or **Access & audit →
Access tokens → New token**.

1. **Name** — what the token is for ("laptop CLI", "release script").
2. **Expires** — presets of 7, 30, 90, 180 days or a year, or a custom date.
   Every token expires; the server rejects lifetimes above
   `auth.max_token_lifetime` (default 366 days).
3. **Repository access** — all repositories, repositories matching a
   pattern, selected repositories, or none; combined with the actions
   read, write, delete, admin.
4. **Administration scopes** (optional) — a level per system area, or
   everything (`system:*`).

The dialog previews what the token can do in plain words and warns about
scopes your roles do not grant (they would have no effect). The secret is
shown **once**; Hangar stores only its SHA-256. Use it as the password with
any username (docker, npm, Maven, pip, NuGet) or as
`Authorization: Bearer hgr_…` for the API.

Token lists show the prefix, scopes, creation date, last use (time and client
address) and expiry. Revoking takes effect immediately, including registry
logins obtained with the token. Administrators with `system:tokens:read` see
everyone's tokens (`GET /tokens?all=true`, filters `owner`, `owner_kind`,
`status`, `q`) and can revoke them with `system:tokens:delete`.

**Expiry warnings**: tokens that expire within
`auth.token_expiry_warning` (default 14 days) are listed in the session
payload (`token_warnings`), shown on My account and under the notification
bell. Holders of `system:tokens:read` also see service-account tokens that
are about to expire.

## Roles and permissions

*Access & audit → Roles*. Built-in roles are read-only. **New role** opens the
privilege editor: add repository privileges (pattern × action) and
administration privileges (area × action); each row is described in plain
words. **Directory groups** map OIDC, LDAP or SAML groups to the role
(`provider` is the identity provider name, `oidc` or `ldap`; `group` the group
name); members of those groups get the role when they sign in. Group
mappings can also live in the configuration (`auth.oidc.group_roles`,
`auth.ldap.group_roles`); see [Single sign-on](sso.md). **Members** lists who has the
role directly and lets you add or remove people
(`POST /roles/{name}/members`, `DELETE /roles/{name}/members/{id}`).

Role privileges are validated: system areas must exist and repository
patterns may only contain repository-name characters and wildcards.

### Effective permissions

`GET /users/{id}/permissions` (drawer → **Permissions**, and *My account →
What you can do*) answers "what may this person do, and why": for every
repository and system area with at least one allowed action, the actions and
each grant behind them — the role and privilege, or the repository's
anonymous read. Disabled and locked accounts are flagged. Roles from
directory group mappings are applied at sign-in and are not part of this
report; token scopes narrow it further. Viewing someone else's report needs
`system:users:read`; everyone may view their own.

## My account

- **Profile** — display name and email of local accounts (`PUT /me/profile`).
- **Password** — `PUT /me/password` with the current and the new password.
  Wrong current passwords count towards the account lockout. Every *other*
  session ends and short-lived registry logins are revoked; personal access
  tokens keep working. Token-authenticated requests cannot change passwords.
- **Signed-in devices** — `GET /me/sessions`; end one
  (`DELETE /me/sessions/{sid}`) or all others (`DELETE /me/sessions`).

## Audit

Every change above is written to the audit log with the actor, client address
and outcome, and every refused attempt is recorded with outcome `denied`.
*Access & audit → Audit log* shows access events by category:

| Category | Actions |
|---|---|
| Users | `user.create`, `user.update`, `user.delete`, `user.disable`, `user.enable`, `user.unlock`, `user.password` |
| Sign-ins | `auth.login`, `auth.login.failed`, `auth.logout`, `auth.token.failed` |
| Tokens | `token.create`, `token.revoke`, `token.issue` |
| Roles | `role.create`, `role.update`, `role.delete`, `role.bind`, `role.unbind` |
| Sessions | `session.revoke`, `session.revoke_all` |

Export any category as CSV or JSON (`GET /audit/export?format=csv&action=user.*`).

## Configuration

```yaml
auth:
  max_token_lifetime: 8784h      # 366 days; longest allowed token expiry
  token_expiry_warning: 336h     # 14 days; warn this long before expiry
  password:
    min_length: 12
  lockout:
    max_attempts: 5              # failed sign-ins (and wrong current passwords)
    window: 15m
    duration: 15m
  session:
    ttl: 12h
    remember_ttl: 720h
```

Sign-in with an OIDC provider or LDAP / Active Directory is configured under
`auth.oidc` and `auth.ldap`; see [Single sign-on](sso.md).

## Operational notes

- Recover access when every administrator is locked out or disabled with
  `hangar admin bootstrap` (see `hangar admin --help`); it only acts when no
  enabled administrator exists.
- Service-account tokens are the right way to give CI access. Do not create
  local members for pipelines.
- Rotate tokens before they expire: create the new one, update the CI secret,
  then revoke the old one; its last-used time shows when nothing uses it any
  more.
