# CI OIDC token exchange (trusted publishing)

CI platforms can prove who a job is: GitHub Actions, GitLab CI and other
OpenID Connect issuers sign a short-lived **ID token** that names the
repository, branch, workflow and environment of the job. Hangar can trade
that ID token for a short-lived Hangar token, so pipelines publish without a
long-lived secret in the CI system.

```
CI job ──ID token──▶ POST /api/v1/auth/oidc/exchange ──▶ hgr_… (15 min, scoped)
                     │ verifies the signature with the issuer's published keys,
                     │ iss / aud / exp / nbf / iat, and matches a trust policy
```

An administrator defines **trust policies**. Each names:

| Field | Meaning |
|---|---|
| `issuer` | The exact `iss` of the ID tokens: `https://token.actions.githubusercontent.com`, `https://gitlab.com` or your GitLab's URL. https only. |
| `audience` | Must be in the token's `aud`. Defaults to the server's `server.base_url`. |
| `conditions` | Claims that must match, all of them: `exact` values or `glob` patterns (`*` any run of characters, `/` included; `?` one character). |
| `service_account` | The enabled service account the minted token acts as. |
| `scopes` | What the minted token may do (it never gets more than the account holds either). |
| `max_ttl_seconds` | Lifetime of minted tokens: default 900 (15 minutes), at most 3600. |

Rules worth knowing:

- Creating, changing or deleting a policy needs `system:settings:admin` **and**
  every privilege in its `scopes` (a policy hands them to whoever holds a
  matching ID token). Reading needs `system:settings:read`.
- A policy needs at least one condition. For `https://token.actions.githubusercontent.com`
  and `https://gitlab.com`, which sign jobs of every account on the platform,
  one condition must pin the owner: `repository`, `repository_owner`,
  `repository_id`, `repository_owner_id` or `sub` on GitHub; `project_path`,
  `namespace_path`, `project_id`, `namespace_id` or `sub` on GitLab, with exact
  values or globs such as `acme/*` (not `acme*`, which also matches `acme-evil/…`).
- Only RS256 and ES256 signatures are accepted; `none` and HMAC are refused.
  Signing keys come from the issuer's discovery document and JWKS over https,
  through the same address guard as sign-in providers (cloud metadata addresses
  are refused). Tokens of issuers no enabled policy names are refused without
  contacting the issuer.
- Each ID token can be exchanged **once** (its `jti` is remembered until it
  expires). Request a new ID token for every exchange.
- Minted tokens are kind `federated`: not listed under Access tokens, not
  rotatable, and they cannot create tokens. `docker login` with one works; the
  registry login ends when the token does. Changing or deleting the policy
  revokes the tokens it minted.
- Exchanges are limited per client address like sign-ins (`auth.login_rate`)
  and audited as `auth.oidc.exchange` with the issuer, subject, policy and
  scopes (never the tokens).

## 1. Service account and trust policy

```bash
# the account minted tokens act as
curl -X POST https://hangar.example.com/api/v1/users \
  -H 'Authorization: Bearer <admin-token>' -H 'Content-Type: application/json' \
  -d '{"username": "ci-publisher", "kind": "service_account", "roles": ["CI deployer"]}'

# GitHub Actions: releases of acme/app from the production environment
curl -X POST https://hangar.example.com/api/v1/trust-policies \
  -H 'Authorization: Bearer <admin-token>' -H 'Content-Type: application/json' \
  -d '{"name": "acme-app-github",
       "issuer": "https://token.actions.githubusercontent.com",
       "audience": "https://hangar.example.com",
       "conditions": [
         {"claim": "repository", "values": ["acme/app"]},
         {"claim": "ref", "match": "glob", "values": ["refs/heads/main", "refs/tags/v*"]},
         {"claim": "environment", "values": ["production"]}
       ],
       "service_account": "ci-publisher",
       "scopes": ["repo:npm-hosted:write", "repo:npm-hosted:read"],
       "max_ttl_seconds": 900}'
```

Useful GitHub claims: `repository`, `repository_owner`, `ref`, `ref_type`
(`branch`/`tag`), `environment`, `workflow_ref`
(`acme/app/.github/workflows/release.yml@refs/heads/main`), `event_name`.

For GitLab use `"issuer": "https://gitlab.com"` (or your instance's URL) and
claims such as `project_path`, `ref`, `ref_type`, `ref_protected` (`"true"`),
`environment`:

```json
{"name": "acme-app-gitlab", "issuer": "https://gitlab.com",
 "conditions": [{"claim": "project_path", "values": ["acme/app"]},
                {"claim": "ref_protected", "values": ["true"]}],
 "service_account": "ci-publisher", "scopes": ["repo:maven-releases:write"]}
```

Any other OIDC issuer works the same way; name the claims it puts in its
tokens (a dotted path reaches into objects, `ext.team`). Booleans compare as
`true`/`false`, numbers as written; arrays never match.

## 2. GitHub Actions

```yaml
permissions:
  id-token: write   # lets the job request an ID token
  contents: read

jobs:
  publish:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - name: Exchange the ID token for a Hangar token
        run: |
          ID_TOKEN=$(curl -sSf -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
            "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://hangar.example.com" | jq -r .value)
          HANGAR_TOKEN=$(curl -sSf -X POST https://hangar.example.com/api/v1/auth/oidc/exchange \
            -H 'Content-Type: application/json' -d "{\"token\": \"$ID_TOKEN\"}" | jq -r .token)
          echo "::add-mask::$HANGAR_TOKEN"
          echo "HANGAR_TOKEN=$HANGAR_TOKEN" >> "$GITHUB_ENV"

      # npm
      - run: |
          echo "//hangar.example.com/repository/npm-hosted/:_authToken=${HANGAR_TOKEN}" > .npmrc
          npm publish --registry https://hangar.example.com/repository/npm-hosted/

      # docker (scopes: repo:docker-hosted:write)
      - run: echo "$HANGAR_TOKEN" | docker login hangar.example.com -u ci --password-stdin

      # pip / twine (scopes: repo:pypi-hosted:write)
      - run: twine upload --repository-url https://hangar.example.com/repository/pypi-hosted/ -u ci -p "$HANGAR_TOKEN" dist/*
```

The `audience` query parameter must equal the policy's `audience`. When
several policies match one token, add `"policy": "<name>"` to the exchange
body.

## 3. GitLab CI

```yaml
publish:
  image: maven:3-eclipse-temurin-21
  id_tokens:
    HANGAR_ID_TOKEN:
      aud: https://hangar.example.com
  rules:
    - if: $CI_COMMIT_REF_PROTECTED == "true"
  script:
    - |
      HANGAR_TOKEN=$(curl -sSf -X POST https://hangar.example.com/api/v1/auth/oidc/exchange \
        -H 'Content-Type: application/json' -d "{\"token\": \"$HANGAR_ID_TOKEN\"}" | jq -r .token)
    - |
      cat > ci-settings.xml <<EOF
      <settings><servers><server>
        <id>hangar</id><username>ci</username><password>${HANGAR_TOKEN}</password>
      </server></servers></settings>
      EOF
    - mvn -B -s ci-settings.xml deploy
        -DaltDeploymentRepository=hangar::https://hangar.example.com/repository/maven-releases/
```

npm, docker and twine take the token as in the GitHub example: any username,
the token as the password or `_authToken`.

## Errors

The exchange answers `401` with `details.reason`:

| Reason | Fix |
|---|---|
| `unknown_issuer` | No enabled policy has this exact `iss` (watch trailing slashes). |
| `no_matching_policy` | The audience or a condition does not match; compare the token's claims with the policy. |
| `expired`, `not_yet_valid` | Request a fresh ID token; check the clocks (one minute of leeway). |
| `replayed` | The ID token was used already; request a new one for every exchange. |
| `unknown_key`, `bad_signature`, `malformed`, `missing_claims` | The token is not a valid RS256/ES256 ID token of the issuer with `exp`, `sub` and `jti`. |

`400 ambiguous_policy`: several policies match; name one in `policy`.
`403 service_account_refused`: the policy's account is disabled. `429`:
too many exchanges from the address (`Retry-After`). `502`: the issuer's keys
could not be fetched.

For local testing with a plain-http issuer, `auth.ci_oidc.allow_http_issuers:
true` (`HANGAR_AUTH_CI_OIDC_ALLOW_HTTP_ISSUERS`) accepts http issuers. Never in
production.
