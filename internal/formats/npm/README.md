# `npm` format

Implements the **npm** format. Protocol requirements and the real-client
acceptance checklist are in [docs/FORMATS.md](../../../docs/FORMATS.md), section
"npm".

Status: **stub** — registered and compiling; handlers answer `501 Not Implemented`.

## Contract

- The package depends only on `internal/core` (types and interfaces) and receives
  everything it uses through `core.Deps`: repositories, components/assets, blob
  stores, upstreams, authentication, authorization, tokens, events, jobs, audit,
  logger and `BaseURL`. It does not touch the database or storage drivers directly.
- Routes: /repository/{repo}/ (scoped names arrive URL-encoded: `@scope%2fname`). The dispatcher has already resolved the repository
  (`core.RepositoryFrom(ctx)`), checked that it is online and of this format.
- Authenticate with `Deps.Auth.Authenticate(ctx, core.CredentialsFromRequest(r))`,
  put the principal in the context with `core.WithPrincipal`, and authorize with
  `Deps.AuthZ.Authorize(ctx, p, core.ActionRead|ActionWrite|ActionDelete, core.RepoResource(repo.Name))`.
  Answer with this format's own challenge/error shape.
- Writes go through `Deps.Components.Publish` (write policy, audit, events, ref counts).
  Reads go through `GetAsset` / `ListComponents` / `ListAssets`, then stream the
  blob from `Deps.Blobs.ForRepository(ctx, repo)`; call `RecordDownload` after a
  successful content response.
- Groups: resolve members with `Deps.Repos.Members` in order. Proxies: fetch with
  `Deps.Upstreams.ForRepository(ctx, repo)` and cache through `Publish`.

## Notes for this format

- packuments are generated metadata: store them as `core.AssetMetadata` or render from components
- tarball URLs in packuments must use `Deps.BaseURL`, never the request Host
- `npm login` / `npm whoami` use `Deps.Auth` and `Deps.Tokens`
- proxy: rewrite upstream tarball URLs to Hangar; honour `ProxyConfig.MetadataTTL`

## Tests

- Unit tests use the in-memory fakes in `internal/core/coretest`:

  ```go
  env := coretest.NewEnv(t)
  repo := env.AddRepo(t, &core.Repository{Name: "npm-hosted", Format: "npm", Type: core.RepoHosted})
  env.Auth.AddUser("alice", "secret")
  env.AuthZ.Grant("alice", "repo:npm-hosted:write")
  h := formats.NewHandler(env.Deps(), npm.New())
  ```

- Real-client acceptance test: `test/e2e/npm_test.go` (build tag `e2e`, run with `make e2e`).
