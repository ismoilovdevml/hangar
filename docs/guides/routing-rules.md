# Routing rules

Routing rules keep internal package names on the repositories that own them.
They protect against **dependency confusion**: someone publishes your internal
name (`Edcom.Common.Utils`, `@trk/ui`, `com.edcom:utils`) with a higher version
on a public registry, and a group of your hosted repository plus a proxy of
that registry serves the public copy to your builds.

Rules attach to proxy and group repositories:

| Repository | Rules | Effect |
|---|---|---|
| proxy | `block`, `allow` | A blocked name is never requested from the remote and never served from the cache, wherever the proxy is used (directly or in any group). With `allow` set, every other name is blocked. `block` wins over `allow`. |
| group | per member: `exclusive`, `block` | `exclusive` reserves names for a member: a name that matches the exclusive patterns of one or more members is served only by them. `block` keeps names away from one member. |

Rules are part of the repository configuration (`config.routing`), so they are
exported and applied with the rest of it. Rule sets are not shared between
repositories on purpose: a pattern only means something for one format, and a
rule that lives on the repository it protects cannot be detached from it by
deleting or renaming another object.

## Which to use

The recommended setup does both:

1. **Block on the public proxy** the names your hosted repositories publish.
   This protects every group the proxy is ever added to, and the names are
   never sent to the public remote (nobody learns them from your requests).
2. **Reserve on the group**: make the names exclusive to the hosted member.
   This is fail-closed: a proxy added to the group later is excluded for those
   names without anyone remembering to add a rule.

```bash
# 1. nuget.org proxy: never fetch internal names
curl -X PUT https://hangar.example.com/api/v1/repositories/nuget-proxy/routing \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"block": ["Edcom.*"]}'

# 2. group: Edcom.* only from the hosted member
curl -X PUT https://hangar.example.com/api/v1/repositories/nuget-group/routing \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"members": [{"member": "nuget-hosted", "exclusive": ["Edcom.*"]}]}'
```

The same as configuration:

```yaml
name: nuget-group
format: nuget
type: group
config:
  group:
    members: [nuget-hosted, nuget-proxy]
  routing:
    members:
      - member: nuget-hosted
        exclusive: ["Edcom.*"]
```

In the web UI: **Repositories → (repository) → Settings → Routing rules**.
**Block names your hosted repositories publish** fills in patterns derived from
the names your hosted repositories already hold (on a group: exclusive to the
hosted member they come from; on a proxy: blocked). Review them and save.

## Patterns

`*` matches any run of characters (including `/`, `.` and `:`), `?` one
character. Each format matches its own package identity:

| Format | Identity | Example patterns |
|---|---|---|
| NuGet | package id, case-insensitive | `Edcom.*` |
| npm | `@scope/name` or `name` | `@trk/*`, `trk-*` |
| Maven | `groupId:artifactId`; a pattern without `:` matches the groupId | `com.edcom`, `com.edcom.*`, `com.edcom:utils-*` |
| PyPI | project name, normalized (case-insensitive; `-`, `_`, `.` equivalent) | `edcom-*` (also matches `Edcom_Utils`) |
| Go | module path; GOPRIVATE syntax: a pattern matches its leading path elements | `git.example.com/corp`, `*.corp.example.com` |
| Cargo | crate name, case-insensitive, `-` and `_` equivalent | `edcom-*` |
| Docker | image path in the repository; a proxy's own rules see the remote's path (`library/nginx` for Docker Hub) | `team/*`, `library/*` |
| Helm | chart name | `edcom-*` |
| APT | package name | `edcom-*` |
| RPM | package name | `edcom-*` |
| raw | file path without the leading `/` | `internal/*` |

Up to 256 patterns per list, 256 bytes each, no spaces.

## What is enforced where

- **Metadata**: merged version lists, registrations, packuments, `maven-metadata.xml`,
  `@v/list` and `@latest`, sparse index files, `index.yaml`, Simple API pages,
  tag lists, Packages and repodata of groups, and search results leave out
  versions from excluded members and names a proxy blocks.
- **Downloads** of an excluded name answer `404`. The body names the rule, and
  the `X-Hangar-Routing` response header carries the reason on every response
  where a rule excluded something:

  ```
  HTTP/1.1 404 Not Found
  X-Hangar-Routing: blocked by routing rule "Edcom.*" on repository "nuget-group" (member "nuget-proxy", reserved for "nuget-hosted")

  v3-flatcontainer/edcom.common.utils/99.0.0/edcom.common.utils.99.0.0.nupkg: not found; blocked by routing rule ...
  ```

- **Proxy requests** for a blocked name are refused before anything is sent to
  the remote, also when the format reaches a related host (file CDN). A search
  whose query is a blocked name is answered from the cache only. A package
  cached before the rule was added is not served either.
- Every remote request of a proxy with rules must declare which package it is
  about; a request that does not is refused (and logged as an error), so a
  format cannot leak a name by accident.

Limits:

- APT and RPM proxies pass the remote's signed `Release` / `repomd.xml` and
  indexes through unchanged (filtering them would break the signature). A
  blocked package still appears in those indexes, but its download answers 404
  without contacting the remote. APT and RPM groups only combine hosted
  members, and their merged indexes are filtered.
- Helm proxies fetch the remote's `index.yaml` as a whole (it lists every
  chart), then leave blocked charts out of what they serve.
- Go: keep internal modules in `GOPRIVATE` / `GONOSUMDB` on clients as well; a
  checksum database lookup of a blocked module through Hangar answers 404.
- Through a group, the `X-Hangar-Routing` header shows rule patterns and
  repository names to anyone who can read the group.

## Test a name

```bash
curl -X POST https://hangar.example.com/api/v1/repositories/nuget-group/routing:test \
  -H "Authorization: Bearer $HANGAR_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name": "Edcom.Common.Utils"}'
```

The answer lists every member (nested groups depth-first) with whether it may
serve the name and the rule that excludes it, and `served_by`, the first hosted
or proxy member that may. The settings page has the same **Test a name** field.

## API

| Call | Permission |
|---|---|
| `GET /api/v1/repositories/{name}/routing` | read |
| `PUT /api/v1/repositories/{name}/routing` | repository admin; empty rules clear them |
| `DELETE /api/v1/repositories/{name}/routing` | repository admin |
| `POST /api/v1/repositories/{name}/routing:test` | read |
| `GET /api/v1/repositories/{name}/routing:suggest` | repository admin; reads only hosted repositories the caller can read |

Rule changes are audited as `repository.routing.update` with the rules before
and after. A group's rules may only name its members: remove a member's rules
before removing the member from the group (the update is refused otherwise).
Full schemas: [api/openapi.yaml](../../api/openapi.yaml).

## Monitoring

- `hangar_routing_blocked_total{format, repository, member, rule}` counts
  exclusions (`repository` holds the rule; `member` is empty for proxy rules).
- Each exclusion is logged at info level and audited as `routing.blocked`
  (outcome `denied`, details: package, rule, pattern, member, the group it was
  reached through), at most once per repository, member and name every 10
  minutes per node.

A rising `hangar_routing_blocked_total` for a proxy block rule means builds ask
for internal names through the public proxy: check that clients use the group,
not the proxy directly.
