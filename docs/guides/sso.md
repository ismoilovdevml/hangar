# Single sign-on: OIDC and LDAP / Active Directory

Hangar can sign people in with an OpenID Connect provider (the **Continue
with SSO** button) and with an LDAP directory or Active Directory (the
directory toggle on the login form, and HTTP Basic from package clients).
Local accounts keep working next to both.

Examples use `https://hangar.example.com` for `server.base_url`,
`https://sso.example.com` for the identity provider and `dc=example,dc=com`
for the directory. Replace them with your own.

## Configure from the web UI or the API

Administrators (`system:settings:admin`, e.g. the built-in `Admin` role)
configure both providers in **Settings → Authentication** without editing
files or restarting. Users with only `system:settings:read` see the same
page read-only.

### Walkthrough: pick a preset

1. **What do you use?** The page opens with eight cards: *Active Directory*,
   *OpenLDAP*, *FreeIPA* (LDAP), *Keycloak*, *Microsoft Entra ID*, *Google
   Workspace*, *Okta* (OIDC) and *Other* (any OIDC provider such as GitLab,
   Authentik or Dex, or any LDAP directory; a switch picks the protocol). The
   card of the provider in use is selected and marked *in use*; the status
   chips next to the title show every sign-in method that is on, and a
   failing one is spelled out under the title.
2. **Fill in three things.** OIDC: the issuer URL (the field shows the
   provider's template, e.g. `https://<host>/realms/<realm>` or
   `https://login.microsoftonline.com/<tenant-id>/v2.0`; Google's issuer is
   filled in), the client ID and the client secret. LDAP: the server, the
   service account (`user@domain` or a DN) and its password.
3. **Check what the preset sets.** *We'll use* lists the scopes, claims or
   attributes the preset chose. **Customize** opens every other field
   (button label, client authentication, scopes, claims, CA bundle; LDAP
   base DNs, filters, attributes, group search, nested groups, StartTLS).
   An error in one of those fields opens them by itself.
4. **Register Hangar at the provider.** The page shows the redirect URI to
   copy (`<server.base_url>/api/v1/auth/oidc/callback`) and, under *How to
   set up …*, where the client and the groups claim are created at that
   provider.
5. **Groups and roles** opens the group → role mapping table, the default
   roles, the allowed groups and auto-create. The footer says what a new
   user gets (*New users get Reader*).
6. **Test**, then **Add Keycloak** (or *Save* for the provider in use). The
   settings apply on every node at once.

What each preset fills in:

| Preset | Fills in |
|---|---|
| Keycloak | scopes `openid profile email`, `preferred_username`, groups from the `groups` claim (Group Membership mapper) |
| Microsoft Entra ID | as Keycloak; Entra sends group **object IDs**, so map those IDs |
| Google Workspace | issuer `https://accounts.google.com`, username from `email`; Google sends no groups (make the OAuth app *Internal* to keep other accounts out) |
| Okta | scopes `openid profile email groups`, groups from the `groups` claim (groups claim filter on the app) |
| Active Directory | `sAMAccountName`, `mail`, `displayName`; nested groups through `LDAP_MATCHING_RULE_IN_CHAIN`; user and group search from the domain root of the service account |
| OpenLDAP | `(&(objectClass=inetOrgPerson)(uid={username}))`, `uid`, `mail`, `cn`; groups from `memberOf` (memberof overlay) |
| FreeIPA | `uid`, `mail`, `displayName`; users under `cn=users,cn=accounts,<root>`; groups from `memberOf` |
| Other | the server defaults; every field is open |

Switching cards keeps what you typed in the credential fields (client ID
and secret, server, service account and password) and the mapping table;
only the preset's own fields change. A search base derived from the
service account follows it until you type your own. The chosen preset is
stored with the settings (`preset`, informational only) so the page reopens
on it; providers saved earlier or set in the configuration file are
recognised by their issuer or filter.

Hangar signs in with one OIDC provider and one directory. Picking another
OIDC card while one is in use offers **Switch to …** and asks for that
provider's own client secret, so the stored secret of the old provider is
never sent to the new one by mistake.

- **Test** checks the values in the form before you save them. OIDC: the
  discovery document, that its `issuer` matches exactly, the signing keys,
  and a `client_credentials` request that verifies the client secret. Many
  providers do not allow that grant for a sign-in client; the test then
  reports *Discovery ok, credentials are verified at first sign-in*. LDAP:
  connect (with StartTLS), service bind and, with a username, a lookup of
  that user, their groups and the roles the mapping grants. No user
  password is needed.
- **Save** validates every field (unknown roles, bad URLs, `<…>` template
  parts left in the issuer, filters without `{username}`, …), stores the
  settings in the database and applies them on every node within seconds:
  the node you saved on at once, the others through the event bus, with a
  30-second poll as a safety net. The login page shows or hides the SSO
  button and the directory toggle accordingly. The switch next to the
  provider's name turns a saved provider off and on.

Secrets (client secret, bind password) are sealed with
`HANGAR_SECRET_KEY` and are write-only: the UI and the API only say whether
one is stored. Leave the field empty to keep it; **Remove** clears it.
Storing a secret needs `HANGAR_SECRET_KEY`; `hangar admin rotate-secrets`
re-wraps these values too. Every change is audited
(`settings.oidc.update`, `settings.ldap.update`) with the names of the
changed fields, never the secret values; tests are audited as
`settings.oidc.test` and `settings.ldap.test`.

**The configuration file wins.** A provider enabled in `hangar.yaml` or the
`HANGAR_AUTH_OIDC_*` / `HANGAR_AUTH_LDAP_*` environment is shown read-only,
labelled *set in configuration file*, and cannot be saved from the UI;
change it in the file and restart. **Test** still works and checks the
file's settings. Other cards of the same protocol show their setup steps
and the redirect URI, with a note that the file has to change. The other provider can still be managed
in the UI. To move a provider from the file to the UI, set
`enabled: false` there (or remove it), restart, and enter it in the UI.

The REST API behind the page (`api/openapi.yaml`):

| Method and path | Purpose |
|---|---|
| `GET /api/v1/identity-providers` | Status of both providers |
| `GET`, `PUT /api/v1/identity-providers/oidc` | Read or replace the OIDC settings |
| `POST /api/v1/identity-providers/oidc:test` | Test (body `{"settings": {...}}` optional) |
| `GET`, `PUT /api/v1/identity-providers/ldap` | Read or replace the LDAP settings |
| `POST /api/v1/identity-providers/ldap:test` | Test (`{"settings": {...}, "username": "jdoe"}`) |

```sh
curl -u admin -X PUT https://hangar.example.com/api/v1/identity-providers/oidc \
  -H 'Content-Type: application/json' -d '{
    "enabled": true, "issuer_url": "https://sso.example.com/realms/acme",
    "client_id": "hangar", "client_secret": "…", "client_auth": "basic",
    "display_name": "Company SSO", "scopes": ["openid", "profile", "email"],
    "username_claim": "preferred_username", "email_claim": "email",
    "name_claim": "name", "groups_claim": "groups", "ca_pem": "", "preset": "keycloak",
    "group_roles": [{"group": "/devops", "roles": ["Publisher"]}],
    "default_roles": ["Reader"], "allowed_groups": [], "auto_create": true}'
```

A `400` answer lists problems by field in `details.fields`
(`"group_roles[0].roles": "role \"Nope\" does not exist"`).

## How external users work

- **Created at first sign-in** (`auto_create: true`, the default) with source
  `oidc` or `ldap`. They have no local password: local sign-in and password
  changes are refused, and the profile (email, display name) is refreshed
  from the provider on every sign-in.
- **Linked by identity, not by name.** An OIDC user is linked to the
  provider's issuer and subject (`iss` + `sub`), so a rename at the provider
  renames the Hangar user. LDAP users are linked by username.
- **Never takes over an account.** If the username already belongs to a
  local member, a service account or another external identity, sign-in
  fails with `account_conflict` and the existing account is untouched.
  Rename one of the two to resolve it.
- **Roles from groups.** `group_roles` maps group names to roles and
  `default_roles` applies to everyone from the provider. Mapped roles are
  re-synced on every sign-in: leaving a group removes its roles at the next
  sign-in. Roles an administrator binds by hand are kept; binding a mapped
  role by hand turns it into a manual one. Roles in *Access & audit → Roles*
  can also map groups with provider `oidc` or `ldap`. Every role named in
  the configuration file must exist, or Hangar refuses to start; the UI
  refuses to save a mapping with unknown roles.
- **Admission.** `allowed_groups` (optional) admits only members of those
  groups. With `auto_create: false` only users who signed in before are
  admitted.
- **Sessions** are ordinary Hangar sessions (same lifetime, CSRF protection,
  *Signed-in devices*). Disabling a user in Hangar blocks their sign-in.
- **Audit**: `auth.login` with `method: oidc` or `ldap`, the roles added and
  removed; `user.create` with `provisioned: true`; failures as
  `auth.login.failed` with a reason.

Group names match case-insensitively. A leading `/` is ignored, so the
Keycloak full path `/devops` matches `devops` (and `/it/devops` matches
`it/devops` or `/it/devops`). For LDAP a group matches by its DN or by its
name (the first RDN value: `CN=Hangar Admins,OU=Groups,...` → `Hangar Admins`).

## OIDC

Hangar uses the authorization code flow with PKCE (S256), checks the
`state`, and verifies the ID token signature, issuer, audience, expiry and
`nonce`. When the ID token lacks the username or groups claim, the userinfo
endpoint fills them in (its `sub` must match).

| Setting | Value |
|---|---|
| Redirect URI | `<server.base_url>/api/v1/auth/oidc/callback` |
| Sign-in start | `GET /api/v1/auth/oidc/start?redirect=/some/page` |
| Scopes | `openid profile email` (do not add `groups`; see below) |

The state, nonce and PKCE verifier travel in a 10-minute, HttpOnly, `Secure`,
`SameSite=Lax` cookie encrypted with a key derived from
`security.secret_key`. Set `HANGAR_SECRET_KEY` on every node (you need it for
stored credentials anyway); without it the key is random per process and a
sign-in has to finish on the node that started it.

If the provider is unreachable when Hangar starts, Hangar starts anyway and
retries discovery on the next sign-in. `GET /api/v1/identity-providers`
shows the provider state.

### Keycloak

1. *Clients → Create client*: type **OpenID Connect**, client ID `hangar`.
2. *Capability config*: **Client authentication: On** (confidential client),
   **Standard flow** only; turn off Direct access grants, Implicit flow,
   Service accounts and Device authorization.
3. *Login settings*:
   - Valid redirect URIs: `https://hangar.example.com/api/v1/auth/oidc/callback`
   - Web origins: `https://hangar.example.com`
4. *Advanced → Advanced settings*: **Proof Key for Code Exchange Code
   Challenge Method: S256**.
5. *Credentials*: copy the client secret (authenticator **Client Id and
   Secret**, which accepts `client_secret_basic` and `client_secret_post`).
6. *Client scopes → hangar-dedicated → Add mapper → By configuration →
   Group Membership*: name `groups`, token claim name `groups`, **Full group
   path: Off**, Add to ID token and Add to userinfo: On. With full path on,
   groups arrive as `/it/devops`; that works too, map the path then.

```yaml
auth:
  oidc:
    enabled: true
    issuer_url: https://sso.example.com/realms/acme
    client_id: hangar
    # client_secret from HANGAR_AUTH_OIDC_CLIENT_SECRET
    client_auth: basic          # or post
    display_name: Acme SSO
    group_roles:
      hangar-admins: [Admin]
      devops: [Publisher]
    default_roles: [Reader]
    allowed_groups: [staff, devops, hangar-admins]
```

```sh
export HANGAR_AUTH_OIDC_CLIENT_SECRET='...'
export HANGAR_SECRET_KEY="$(openssl rand -base64 32)"   # once; same on every node
```

Keep `scopes` at `openid profile email`. Some realms reject a `groups` scope
with `invalid_scope`; the Group Membership mapper above puts the groups into
the token without one.

Other providers work the same way: register the redirect URI, give Hangar
the issuer URL, client ID and secret, and make the groups reach a claim
(`groups_claim`, a dotted path such as `realm_access.roles` is allowed). If
the username is not in `preferred_username`, set `username_claim` (for
example `email` or `upn`); it must be a valid Hangar username.

## LDAP and Active Directory

A sign-in binds as the service account, searches `user_base_dn` for exactly
one entry matching `user_filter` (the login name is escaped), binds as that
entry with the password, then reads the groups. Empty passwords are refused
before anything is sent (an empty bind password is an anonymous bind on many
directories). Each sign-in uses its own connection with `timeout`.

The same lockout as for local accounts applies to directory users who signed
in before (`auth.lockout`), and the login rate limit applies per address.
Package clients (Docker, Maven, npm, pip, NuGet) can use the directory
password with HTTP Basic; verified credentials are cached for
`auth.basic_cache_ttl`. Tokens remain the better choice for CI.

### Active Directory

```yaml
auth:
  ldap:
    enabled: true
    url: ldaps://dc1.example.com:636
    bind_dn: CN=svc-hangar,OU=Service Accounts,DC=example,DC=com
    # bind_password from HANGAR_AUTH_LDAP_BIND_PASSWORD
    user_base_dn: OU=People,DC=example,DC=com
    # defaults: (&(objectClass=user)(sAMAccountName={username})),
    # sAMAccountName / mail / displayName, groups from memberOf
    group_roles:
      Hangar Admins: [Admin]
      CN=Hangar Publishers,OU=Groups,DC=example,DC=com: [Publisher]
    default_roles: [Reader]
```

To include nested groups, search them with `LDAP_MATCHING_RULE_IN_CHAIN`:

```yaml
    group_base_dn: OU=Groups,DC=example,DC=com
    nested_groups: true
```

To skip disabled AD accounts in the search itself, use
`(&(objectClass=user)(sAMAccountName={username})(!(userAccountControl:1.2.840.113556.1.4.803:=2)))`.

### OpenLDAP

```yaml
auth:
  ldap:
    enabled: true
    url: ldap://ldap.example.com:389
    start_tls: true
    ca_file: /etc/hangar/ldap-ca.pem
    bind_dn: cn=hangar,ou=services,dc=example,dc=com
    user_base_dn: ou=people,dc=example,dc=com
    user_filter: (&(objectClass=inetOrgPerson)(uid={username}))
    username_attribute: uid
    name_attribute: cn
    group_base_dn: ou=groups,dc=example,dc=com
    group_filter: (&(objectClass=groupOfNames)(member={dn}))
```

## Troubleshooting

After a failed single sign-on the browser lands on `/login?error=<code>`.
The audit log (`auth.login.failed`) and the server log carry the detail.

| Code / symptom | Meaning and fix |
|---|---|
| `sso_unavailable` | Discovery failed: check `issuer_url` (it must equal the `issuer` in `<issuer_url>/.well-known/openid-configuration`, including any trailing slash), DNS, the proxy environment and TLS (`ca_file`). |
| `sso_state` | The state cookie was missing, expired (10 minutes) or for another sign-in. Start again; on several nodes, set the same `HANGAR_SECRET_KEY` everywhere; behind plain HTTP the `Secure` cookie is dropped (use HTTPS, or `auth.session.insecure` for development only). |
| `sso_failed` | Code exchange or token check failed. Reasons in the log: `code_exchange` (wrong client secret or `client_auth`, redirect URI mismatch), `id_token_invalid` (audience is not `client_id`, issuer mismatch, unknown signing key), `id_token_expired` (clock skew), `nonce_mismatch`. |
| `access_denied` | The provider refused or the user cancelled. |
| `group_denied` | The user is in none of `allowed_groups`. Check that the groups claim reaches Hangar (mapper on the ID token or userinfo). |
| `account_conflict` | The username belongs to another Hangar account. |
| `not_provisioned` | `auto_create` is off and the user never signed in. |
| `invalid_profile` | The username claim is missing or not a valid username; set `username_claim`. |
| `account_disabled` | An administrator disabled the user in Hangar. |
| LDAP: "Wrong username or password" for everyone | Check `user_base_dn` and `user_filter` with `ldapsearch -H ldaps://dc1.example.com -D "$BIND_DN" -W -b "OU=People,DC=example,DC=com" "(sAMAccountName=jdoe)"`. A filter matching more than one entry is refused. |
| LDAP: 503 "directory cannot be reached" | Network, TLS or service account problem; `GET /api/v1/identity-providers` shows the last error. |
| LDAP: 409 conflict | A local member has that username; LDAP never signs in as a local account. |
| Roles do not change | Mapped roles change at the next sign-in. Package clients using a cached Basic login pick changes up after `auth.basic_cache_ttl`. |

An LDAP integration test runs against a real directory when
`HANGAR_TEST_LDAP_URL` is set (see `internal/auth/ldap/ldap_test.go`).
