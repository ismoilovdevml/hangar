# `helm` format

Implements classic **Helm chart repositories** (an `index.yaml` plus chart
archives) for hosted, proxy and group repositories. Protocol notes and the
acceptance checklist are in [docs/FORMATS.md](../../../docs/FORMATS.md),
section "Helm". Spec: <https://helm.sh/docs/topics/chart_repository/>. OCI
charts are served by the `docker` format.

Client URL: `https://<hangar>/repository/<repo>`.

## Endpoints (relative to `/repository/{repo}/`)

| Method | Path | Resource |
|---|---|---|
| GET/HEAD | `index.yaml` | generated (hosted, group) or rewritten upstream (proxy) index |
| GET/HEAD | `{name}/{version}/{name}-{version}.tgz` | chart archive |
| GET/HEAD | `{name}/{version}/{name}-{version}.tgz.prov` | provenance file |
| PUT | `{name}-{version}.tgz` / `.tgz.prov` | upload (hosted, group write member) |
| POST | `api/charts` | upload, raw body or multipart `chart` / `prov` (`?force`) |
| POST | `api/prov` | provenance upload |
| DELETE | `api/charts/{name}/{version}` | delete a version |
| GET | `api/charts`, `api/charts/{name}`, `api/charts/{name}/{version}` | ChartMuseum-compatible read API |

`index.yaml` carries `serverInfo.contextPath`, so `helm cm-push <chart> <repo>`
finds the upload API without extra flags. Chart URLs in the index are relative,
so one index works through groups and clients send credentials on downloads.

## Files

- `helm.go`: format registration and routing.
- `hosted.go`, `chart.go`, `prov.go`, `version.go`: uploads, `Chart.yaml` validation, provenance, versions.
- `index.go`: index rendering and the event-invalidated cache.
- `proxy.go`: upstream index fetch, URL rewriting, archive caching.
- `read.go`: downloads and the ChartMuseum read API.
