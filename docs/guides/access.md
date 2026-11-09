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
can be read by everyone, signed in or not. A request that sends credentials must
send valid ones, though: a wrong, expired or revoked token or password answers
`401` on every endpoint, public ones included, rather than falling back to
anonymous access.

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

1. **Name** — what the token is for ("laptop CLI", "release pipeline").
2. **Expires** — 7, 30 or 90 days, a year, or a custom date. Every token
   expires; the server rejects lifetimes above `auth.max_token_lifetime`
   (default 366 days).
3. **Permissions** — a preset:
   - *Read only*: download from every repository.
   - *Publish to selected repositories*: download and publish to the
     repositories and patterns (`npm-*`) you pick.
   - *Repository admin*: manage the picked repositories.
   - *Security reviewer*: read vulnerability findings and waive them.
   - *Full admin*: everything (`system:*`); the dialog warns before you use it.
   - *Custom*: repositories (names or patterns) × read, write, delete, admin,
     plus read, write or admin per administration area.
4. **Restrictions** (optional) — the addresses or CIDR networks the token may
   be used from, and a read-only flag that limits it to downloading and
   viewing whatever its permissions say.

A token never does more than its owner: requests need a privilege from the
owner's roles **and** a matching scope, checked on every request. The dialog
greys out what the owner lacks (presets, repositories, actions, area levels)
and says why. Below the form a live summary reads the token in plain words:
"Can publish to nuget-internal", "Can download from npm-*", "Cannot manage
users & roles, tokens, settings and repositories".

The secret is shown **once**; Hangar stores only its SHA-256. The same panel
offers ready-to-paste snippets with this server's real URL: a curl call with
the Bearer header, `HANGAR_URL` / `HANGAR_TOKEN` lines, client configuration
for npm, NuGet, pip, Maven and docker for repositories the token can reach,
and a block to give to an automation, with links to `/llms.txt` and the
OpenAPI document. See [the API guide](api.md) for automating Hangar.

Token lists show the prefix, what the token can do, restriction badges,
creation date, last use (time and client address) and expiry. **Rotate**
creates a replacement with the same name, permissions, restrictions and
lifetime and shows its secret; the old token keeps working for a grace period
you choose (1 hour to 7 days), is revoked at once, or is kept until it
expires. **Revoke** takes effect immediately, including registry logins
obtained with the token. Administrators with `system:tokens:read` see
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

Open it from the avatar menu or from **Settings → My account** (`/account`).

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
| Tokens | `token.create`, `token.rotate`, `token.revoke`, `token.issue` |
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
  enabled administrator exists. `hangar admin reset-password --user NAME`
  gives a local user a one-time password they must replace at the next
  sign-in.
- **Require password change** (member actions) makes a local member set a new
  password before they can do anything else; it is audited as
  `user.password.require_change`. Their access tokens keep working.
- Service-account tokens are the right way to give CI access. Do not create
  local members for pipelines.
- Rotate tokens before they expire with **Rotate** (or
  `POST /tokens/{id}/rotate`): pick a grace period, update the CI secret, and
  the old token stops working on its own. Its last-used time shows whether
  anything still uses it.
- Restrict long-lived automation tokens to the runners' networks, and make
  tokens read-only when the job only downloads.
