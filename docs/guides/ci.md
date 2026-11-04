# CI integration

CI systems are ordinary Hangar clients: they authenticate with a token stored as a
masked secret. Hangar needs no plugin or callback into the CI system.

Placeholders:

- `hangar.example.com`: the host of `server.base_url`.
- `<admin-token>`: a token of an administrator (needs `system:users:write` and
  `system:tokens:write`, for example an `Admin`'s token with scope `system:*`).
- `ci-publisher`: the service account the pipelines use.
- `HANGAR_TOKEN`: the secret / masked variable holding the service account's token.

## 1. Create a service account and a scoped token

A service account is a non-human user without a password; it authenticates only
with tokens. Give it a role, then issue tokens that are narrower still.

```bash
# service account with the built-in "CI deployer" role
# (read every repository, publish and delete in hosted repositories)
curl -X POST https://hangar.example.com/api/v1/users \
  -H 'Authorization: Bearer <admin-token>' -H 'Content-Type: application/json' \
  -d '{"username": "ci-publisher", "kind": "service_account",
       "display_name": "CI publisher", "roles": ["CI deployer"]}'

# token owned by the service account, limited to two repositories, expiring in 90 days
curl -X POST https://hangar.example.com/api/v1/tokens \
  -H 'Authorization: Bearer <admin-token>' -H 'Content-Type: application/json' \
  -d '{"name": "gitlab-ci", "owner": "ci-publisher",
       "scopes": ["repo:npm-hosted:read", "repo:npm-hosted:write",
                  "repo:npm-group:read"],
       "expires_at": "2027-02-01T00:00:00Z"}'
```

Copy `secret` from the response into your CI system's secret store; it is shown
once. The same can be done on the server host with
`hangar admin token create --user ci-publisher --name gitlab-ci --scopes 'repo:npm-hosted:read,repo:npm-hosted:write' --expires 90d`.

Recommendations:

- One service account per pipeline family, one token per CI system, so a leak can
  be revoked without breaking everything else (`DELETE /api/v1/tokens/<id>`).
- Scope tokens to the repositories the job touches: `repo:<repo>:read` to resolve,
  `repo:<repo>:write` to publish, `repo:<repo>:delete` only for cleanup jobs.
  Patterns work: `repo:npm-*:write`.
- Always set an expiry (at most 366 days) and rotate before it.
- Publish to the hosted repository URL; resolve through the group.
- See [access.md](access.md) for roles and privileges.

The examples below publish a Docker image, an npm package and a Maven artifact.
Each assumes the matching repositories exist (see the client guides) and that
`HANGAR_TOKEN` is a masked secret. A single token for all three jobs needs
`read` and `write` on `docker-hosted`, `npm-hosted` and `maven-releases`, plus
`read` on `npm-group` and `maven-public`; separate tokens per job are better.

## GitLab CI

Add `HANGAR_TOKEN` under Settings, CI/CD, Variables with "Masked" (and
"Protected" if only protected branches publish).

```yaml
variables:
  HANGAR_HOST: hangar.example.com
  HANGAR_USER: ci-publisher

docker-push:
  image: docker:27
  services: [docker:27-dind]
  script:
    - echo "$HANGAR_TOKEN" | docker login "$HANGAR_HOST" -u "$HANGAR_USER" --password-stdin
    - docker build -t "$HANGAR_HOST/docker-hosted/team/app:$CI_COMMIT_SHORT_SHA" .
    - docker push "$HANGAR_HOST/docker-hosted/team/app:$CI_COMMIT_SHORT_SHA"

npm-publish:
  image: node:22
  script:
    # npm expands ${HANGAR_TOKEN} itself; the secret never lands in the file
    - echo "//$HANGAR_HOST/repository/npm-group/:_authToken=\${HANGAR_TOKEN}" > .npmrc
    - echo "//$HANGAR_HOST/repository/npm-hosted/:_authToken=\${HANGAR_TOKEN}" >> .npmrc
    - npm ci --registry "https://$HANGAR_HOST/repository/npm-group/"
    - npm publish --registry "https://$HANGAR_HOST/repository/npm-hosted/"

maven-deploy:
  image: maven:3-eclipse-temurin-21
  script:
    - |
      cat > ci-settings.xml <<'EOF'
      <settings>
        <servers>
          <server>
            <id>hangar</id>
            <username>${env.HANGAR_USER}</username>
            <password>${env.HANGAR_TOKEN}</password>
          </server>
        </servers>
        <mirrors>
          <mirror><id>hangar</id><mirrorOf>*</mirrorOf>
            <url>https://hangar.example.com/repository/maven-public/</url></mirror>
        </mirrors>
      </settings>
      EOF
    - mvn -B -s ci-settings.xml deploy
```

The Maven job expects `distributionManagement` with repository id `hangar` in the
POM (see [maven.md](maven.md)), or pass the target on the command line:
`-DaltDeploymentRepository=hangar::https://hangar.example.com/repository/maven-releases/`.

The npm job needs a token with read on `npm-group` and write on `npm-hosted`.

## GitHub Actions

Store the token as a repository or organization secret named `HANGAR_TOKEN`.

```yaml
name: publish
on:
  push:
    tags: ["v*"]

env:
  HANGAR_HOST: hangar.example.com

jobs:
  docker:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/setup-buildx-action@v3
      - uses: docker/login-action@v3
        with:
          registry: ${{ env.HANGAR_HOST }}
          username: ci-publisher
          password: ${{ secrets.HANGAR_TOKEN }}
      - uses: docker/build-push-action@v6
        with:
          push: true
          platforms: linux/amd64,linux/arm64
          tags: ${{ env.HANGAR_HOST }}/docker-hosted/team/app:${{ github.ref_name }}

  npm:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: 22
          registry-url: https://hangar.example.com/repository/npm-hosted/
      - run: npm ci
      - run: npm publish
        env:
          NODE_AUTH_TOKEN: ${{ secrets.HANGAR_TOKEN }}

  maven:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-java@v4
        with:
          distribution: temurin
          java-version: 21
          server-id: hangar                 # must match the id in distributionManagement
          server-username: HANGAR_USER      # names of environment variables, not values
          server-password: HANGAR_TOKEN
      - run: mvn -B deploy
        env:
          HANGAR_USER: ci-publisher
          HANGAR_TOKEN: ${{ secrets.HANGAR_TOKEN }}
```

`setup-node` writes an `.npmrc` with `registry=` and
`//hangar.example.com/repository/npm-hosted/:_authToken=${NODE_AUTH_TOKEN}`; in this
example `npm ci` therefore also installs from `npm-hosted`. To install through the
group instead, point `registry-url` at `npm-group` and publish with
`npm publish --registry https://hangar.example.com/repository/npm-hosted/` after
adding an auth line for that URL.

For Gradle, pass the credentials as project properties:
`ORG_GRADLE_PROJECT_hangarUsername` and `ORG_GRADLE_PROJECT_hangarPassword`
(see [maven.md](maven.md#gradle)).

## Gitea Actions

Gitea Actions runs GitHub Actions syntax with `act_runner`. Store the token under
the repository's Settings, Actions, Secrets as `HANGAR_TOKEN`; the workflow above
works unchanged in `.gitea/workflows/publish.yaml`. A minimal shell-only variant:

```yaml
name: publish
on: [push]
jobs:
  docker:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Push image
        env:
          HANGAR_TOKEN: ${{ secrets.HANGAR_TOKEN }}
        run: |
          echo "$HANGAR_TOKEN" | docker login hangar.example.com -u ci-publisher --password-stdin
          docker build -t hangar.example.com/docker-hosted/team/app:${GITHUB_SHA::8} .
          docker push hangar.example.com/docker-hosted/team/app:${GITHUB_SHA::8}
```

## Jenkins (declarative pipeline)

Create a "Username with password" credential with id `hangar-ci`: username
`ci-publisher`, password the token. The Credentials Binding plugin masks it in the
log.

```groovy
pipeline {
  agent any
  environment {
    HANGAR_HOST = 'hangar.example.com'
  }
  stages {
    stage('Docker push') {
      steps {
        withCredentials([usernamePassword(credentialsId: 'hangar-ci',
                                          usernameVariable: 'HANGAR_USER',
                                          passwordVariable: 'HANGAR_TOKEN')]) {
          sh '''
            echo "$HANGAR_TOKEN" | docker login "$HANGAR_HOST" -u "$HANGAR_USER" --password-stdin
            docker build -t "$HANGAR_HOST/docker-hosted/team/app:$BUILD_NUMBER" .
            docker push "$HANGAR_HOST/docker-hosted/team/app:$BUILD_NUMBER"
          '''
        }
      }
    }
    stage('npm publish') {
      steps {
        withCredentials([usernamePassword(credentialsId: 'hangar-ci',
                                          usernameVariable: 'HANGAR_USER',
                                          passwordVariable: 'HANGAR_TOKEN')]) {
          sh '''
            echo "//$HANGAR_HOST/repository/npm-hosted/:_authToken=\\${HANGAR_TOKEN}" > .npmrc
            npm publish --registry "https://$HANGAR_HOST/repository/npm-hosted/"
          '''
        }
      }
    }
    stage('Maven deploy') {
      steps {
        withCredentials([usernamePassword(credentialsId: 'hangar-ci',
                                          usernameVariable: 'HANGAR_USER',
                                          passwordVariable: 'HANGAR_TOKEN')]) {
          // ci-settings.xml as in the GitLab example, reading ${env.HANGAR_USER} / ${env.HANGAR_TOKEN}
          sh 'mvn -B -s ci-settings.xml deploy'
        }
      }
    }
  }
}
```

Use single-quoted `sh '''...'''` blocks so Groovy does not interpolate the secret
into the command line.

## Other formats in CI

- PyPI: `uv publish` with `UV_PUBLISH_USERNAME=__token__` and
  `UV_PUBLISH_PASSWORD=$HANGAR_TOKEN`, or `twine upload --repository-url ... -u __token__ -p "$HANGAR_TOKEN"`;
  see [pypi.md](pypi.md).
- NuGet: `dotnet nuget push ... -k "$HANGAR_TOKEN"`; see [nuget.md](nuget.md).
- Raw: `curl -fsS -H "Authorization: Bearer $HANGAR_TOKEN" -T file URL`; see [raw.md](raw.md).
