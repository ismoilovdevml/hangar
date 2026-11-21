# Install with Helm

The chart in `deploy/helm/hangar` runs Hangar as a stateless Deployment with
two or more replicas against an external PostgreSQL and S3. PostgreSQL also
carries the events between pods and the job queue: there is no other
stateful service. Every value is described in
[`values.yaml`](../../deploy/helm/hangar/values.yaml) and checked by
`values.schema.json`; unsupported combinations fail at render time with an
explanation.

## Prerequisites

- Kubernetes 1.27+ (1.30+ for the pre-stop sleep that drains endpoints before
  shutdown), Helm 3.8+.
- PostgreSQL 16+ reachable from the cluster: a managed service,
  CloudNativePG, or similar. Create a database and an owner role
  ([requirements.md](requirements.md)). Allow `replicas x
  (database.max_conns + 17)` connections; a PgBouncer in front must use
  session pooling, not transaction pooling
  ([high-availability.md](high-availability.md#what-each-node-needs)).
- S3-compatible storage and a bucket, or a ReadWriteMany storage class for
  `storage.kind=fs` with more than one replica.
- An ingress controller (ingress-nginx, Traefik, ...) or a Gateway API
  implementation, and certificates (for example cert-manager).

## The chart

Each release publishes the chart as an OCI artifact, signed like the images
([verify a release](verify-release.md#helm-chart)). Its version and
`appVersion` are the release version, so by default it installs the images of
that release:

```bash
helm show values oci://ghcr.io/ismoilovdevml/charts/hangar --version <version>
```

The OCI chart exists from v0.2.0 on. For an earlier release, or to install
from a checkout, use the path `deploy/helm/hangar` of that tag wherever this
page says `oci://ghcr.io/ismoilovdevml/charts/hangar --version <version>`.

## Minimal production install

Create the secrets out of band (or with your secret manager / External
Secrets):

```bash
kubectl create namespace hangar

kubectl -n hangar create secret generic hangar-db \
  --from-literal=database-url='postgres://hangar:PASSWORD@pg.db.svc:5432/hangar?sslmode=require'

kubectl -n hangar create secret generic hangar-s3 \
  --from-literal=s3-access-key-id=AKIA... \
  --from-literal=s3-secret-access-key=...

kubectl -n hangar create secret generic hangar-keys \
  --from-literal=secret-key="$(openssl rand -base64 32)" \
  --from-literal=docker-token-key="$(openssl rand -base64 48)"

kubectl -n hangar create secret generic hangar-admin \
  --from-literal=admin-username=admin \
  --from-literal=admin-password="$(openssl rand -base64 18)"
```

`values-prod.yaml`:

```yaml
# image.tag defaults to the chart's appVersion: `--version` picks the release.
# Pin the verified digest with image.digest (verify-release.md).
replicaCount: 3

config:
  server:
    base_url: https://hangar.example.com
    # Pod CIDR of the ingress controller; the default trusts all private ranges.
    trusted_proxies: [10.244.0.0/16]

database:
  existingSecret: hangar-db

storage:
  kind: s3
  s3:
    endpoint: ""            # empty for AWS; https://minio.example.com for MinIO
    region: eu-central-1
    bucket: hangar-blobs
    existingSecret: hangar-s3

secretKey:
  existingSecret: hangar-keys
dockerTokenKey:
  existingSecret: hangar-keys
bootstrapAdmin:
  existingSecret: hangar-admin

ingress:
  enabled: true
  className: nginx
  preset: nginx
  host: hangar.example.com
  registryHost: registry.example.com
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt

metrics:
  serviceMonitor:
    enabled: true
networkPolicy:
  enabled: true
  ingressFrom:
    - namespaceSelector:
        matchLabels: { kubernetes.io/metadata.name: ingress-nginx }
    - namespaceSelector:
        matchLabels: { kubernetes.io/metadata.name: monitoring }
```

```bash
helm upgrade --install hangar oci://ghcr.io/ismoilovdevml/charts/hangar --version <version> \
  -n hangar -f values-prod.yaml --wait
kubectl -n hangar rollout status deploy/hangar
helm test hangar -n hangar        # the Service answers /readyz
```

The release notes printed by Helm repeat the URL, the Docker login host and
how to create the first administrator. Remove `bootstrapAdmin` from the values
after the first sign-in ([first-admin.md](first-admin.md)).

## What gets deployed

| Resource | Notes |
|---|---|
| Deployment | `replicaCount` or the HPA. Read-only root filesystem, non-root (65532), all capabilities dropped, seccomp `RuntimeDefault`, no service account token. Probes: startup and liveness on `/healthz`, readiness on `/readyz` (database, blob store, event listener). `HANGAR_SERVER_NODE_URL=http://$(POD_IP):8080`, where the other pods read its System logs (unless `config.server.node_url` is set, which is refused with more than one replica). Soft pod anti-affinity by host (`podAntiAffinityPreset: hard` to require it), `topologySpreadConstraints` for zones. Rolling updates with `maxUnavailable: 0`. Pods restart when the config or secret values change. |
| ConfigMap | `hangar.yaml` rendered from `config`. The chart sets `server.addr`, `database.migrate_on_start` and `storage.*` itself, drops a leftover `nats` section, and refuses secrets placed in `config`. |
| Secret | Only what no `existingSecret` provides. Generated values (`secretKey`, `dockerTokenKey`) are created once and kept on upgrades; the Secret is kept on `helm uninstall`. |
| Job (hook) | `hangar migrate up` before install and before every upgrade (`migrations.mode: job`), so pods never start on an old schema and only one process migrates. It gets the server's `extraEnv`, `extraEnvFrom`, `extraVolumes`, `extraVolumeMounts`, `podLabels` and `podAnnotations`, and runs under `serviceAccount.name` when `serviceAccount.create` is false (otherwise the namespace's default ServiceAccount: the chart's own does not exist yet on the first install). |
| Service | Port 80, plus `service.connectorPorts` for Docker port connectors. |
| Ingress / HTTPRoute | The UI host, an optional registry host (`/v2` only), and the metrics path blocked publicly (`blockMetrics`). Presets for ingress-nginx and Traefik ([reverse-proxy.md](reverse-proxy.md)). |
| PodDisruptionBudget | `minAvailable: 1` with two or more replicas. |
| HorizontalPodAutoscaler | `autoscaling.enabled`; scales in by one pod at most every two minutes. |
| ServiceMonitor | Prometheus Operator scraping `/metrics` on every pod. |
| Trivy Deployment, scan adapter Deployment | `imageScanning.enabled`: the Trivy server (database on a PersistentVolumeClaim) and the scan adapter, with their Services and the adapter token Secret ([image scanning](#image-scanning)). |
| NetworkPolicy | Ingress to Hangar only from `networkPolicy.ingressFrom` (any source when empty), plus the other Hangar pods (System logs), the chart's `helm test` pod and, with image scanning, the scan adapter. With image scanning the Trivy server accepts only Hangar and the scan adapter, and the scan adapter only Hangar. Optional egress restriction (`networkPolicy.egress`): DNS, the other Hangar pods on 8080 and the rules you list. PostgreSQL, the blob store, and the remotes of proxy repositories (or the corporate proxy) need to be in that list. |

## Choosing modes

| Value | Options |
|---|---|
| `storage.kind` | `s3` (recommended). `fs` with a PersistentVolumeClaim: `ReadWriteOnce` for one replica (the Deployment switches to `Recreate`), `ReadWriteMany` for more. The claim is kept on uninstall. |
| `storage.s3` credentials | `existingSecret` / `accessKeyID`+`secretAccessKey`, or none: the AWS default chain (IRSA, EKS Pod Identity) with `serviceAccount.annotations`. |
| `migrations.mode` | `job` (default), `onStart` (every pod migrates on start; an advisory lock lets one apply and the others wait), `none` (you run `hangar migrate up`). |
| `metrics.auth.mode` | `none` (default), `bearer` or `basic`, with the credentials in `metrics.auth.existingSecret`; the ServiceMonitor sends them. See [monitoring.md](../operations/monitoring.md#protecting-metrics). |
| `config.server.drain_delay` | Time a terminating pod keeps serving with `/readyz` failing (default `0s`; the pre-stop sleep covers Kubernetes 1.30+). Keep `terminationGracePeriodSeconds` above `lifecycle.preStopSleepSeconds + drain_delay + shutdown_timeout`. See [high-availability.md](high-availability.md#draining-nodes). |

## GitOps (Argo CD, Flux)

Tools that render the chart offline cannot read existing Secrets, so values
generated by the chart would change on every render. Provide all of them:
`secretKey.existingSecret` (or `value`), `dockerTokenKey.existingSecret`,
plus `database.existingSecret` and `storage.s3.existingSecret`. Argo CD runs the
migration hook as a `PreSync` hook.

## Image scanning

```yaml
imageScanning:
  enabled: true
  platforms: all                                   # or default: linux/amd64 only
  httpsProxy: http://proxy.corp.example:3128       # the database downloads
  adapterToken:
    existingSecret: hangar-scanner                 # key scanner-adapter-token; generated when empty
  trivy:
    persistence: { size: 5Gi, storageClass: "" }
    extraEnv:                                      # an internal mirror of the database
      - { name: TRIVY_DB_REPOSITORY, value: registry.corp.example/aquasecurity/trivy-db:2 }
```

The chart runs the Trivy server (`aquasec/trivy`, pinned in `values.yaml`)
and the scan adapter (`ghcr.io/ismoilovdevml/hangar-scanner`, the chart's
`appVersion`), and sets `scanner.*` on Hangar: the adapter pulls images from
Hangar's Service by digest with a registry token valid for minutes. Leave
`config.scanner` empty. With a NetworkPolicy and egress restrictions, the
Trivy pod needs `mirror.gcr.io:443` (or your mirror) through the proxy.
Details: [vulnerabilities.md](../guides/vulnerabilities.md#container-images).

## Private CA and corporate proxy

Mount the CA bundle and point Hangar at it:

```yaml
extraVolumes:
  - name: corp-ca
    configMap: { name: corp-ca-bundle }
extraVolumeMounts:
  - name: corp-ca
    mountPath: /etc/hangar/ca
    readOnly: true
config:
  http:
    proxy: http://proxy.corp.example:3128
    proxy_username: hangar
    no_proxy: [.corp.example, 10.0.0.0/8, .svc, .cluster.local]
    ca_file: /etc/hangar/ca/ca.crt
httpProxyPassword:
  existingSecret: hangar-proxy
storage:
  s3:
    caFile: /etc/hangar/ca/ca.crt   # S3 endpoint with a private certificate
```

## Checking the chart

`make helm-lint` and `make helm-template` lint and render the chart with
every value set in `deploy/helm/hangar/ci/` (HA with S3 and network
policies; autoscaling with IRSA and a leftover 0.1.x `nats` section; single
node with a volume and a fixed node URL; two replicas on ReadWriteMany), and
validate the manifests with kubeconform when it is installed
(`KUBECONFORM_REQUIRED=1` makes it mandatory, as in CI). Plain `helm lint` without
values fails on purpose: `config.server.base_url` and a database are
required.

## Upgrading from 0.1.x

Chart 0.2.0 no longer deploys NATS, and `nats.*` values are ignored (the
schema still accepts them, and the release notes print a reminder to remove
them). The bundled NATS StatefulSet is deleted by the upgrade; its volumes are
not. See [upgrade.md](upgrade.md#upgrading-to-020) for the whole procedure.
