# Install with Helm

The chart in `deploy/helm/hangar` runs Hangar as a stateless Deployment with
two or more replicas against an external PostgreSQL and S3, and can deploy a
3-node NATS JetStream cluster for it. Every value is described in
[`values.yaml`](../../deploy/helm/hangar/values.yaml) and checked by
`values.schema.json`; unsupported combinations fail at render time with an
explanation.

## Prerequisites

- Kubernetes 1.27+ (1.30+ for the pre-stop sleep that drains endpoints before
  shutdown), Helm 3.8+.
- PostgreSQL 16+ reachable from the cluster: a managed service,
  CloudNativePG, or similar. Create a database and an owner role
  ([requirements.md](requirements.md)).
- S3-compatible storage and a bucket, or a ReadWriteMany storage class for
  `storage.kind=fs` with more than one replica.
- An ingress controller (ingress-nginx, Traefik, ...) or a Gateway API
  implementation, and certificates (for example cert-manager).

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
image:
  tag: "<version>"          # pin a release
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

nats:
  mode: bundled             # 3-node JetStream cluster with persistent volumes

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
helm upgrade --install hangar deploy/helm/hangar -n hangar -f values-prod.yaml --wait
kubectl -n hangar rollout status deploy/hangar
helm test hangar -n hangar        # the Service answers /readyz
```

The release notes printed by Helm repeat the URL, the Docker login host and
how to create the first administrator. Remove `bootstrapAdmin` from the values
after the first sign-in ([first-admin.md](first-admin.md)).

## What gets deployed

| Resource | Notes |
|---|---|
| Deployment | `replicaCount` or the HPA. Read-only root filesystem, non-root (65532), all capabilities dropped, seccomp `RuntimeDefault`, no service account token. Probes: startup and liveness on `/healthz`, readiness on `/readyz` (database, blob store, NATS). Soft pod anti-affinity by host (`podAntiAffinityPreset: hard` to require it), `topologySpreadConstraints` for zones. Rolling updates with `maxUnavailable: 0`. Pods restart when the config or secret values change. |
| ConfigMap | `hangar.yaml` rendered from `config`. The chart sets `server.addr`, `database.migrate_on_start`, `storage.*` and `nats.*` itself and refuses secrets placed in `config`. |
| Secret | Only what no `existingSecret` provides. Generated values (`secretKey`, `dockerTokenKey`, the NATS password) are created once and kept on upgrades; the Secret is kept on `helm uninstall`. |
| Job (hook) | `hangar migrate up` before install and before every upgrade (`migrations.mode: job`), so pods never start on an old schema and only one process migrates. |
| Service | Port 80, plus `service.connectorPorts` for Docker port connectors. |
| Ingress / HTTPRoute | The UI host, an optional registry host (`/v2` only), and the metrics path blocked publicly (`blockMetrics`). Presets for ingress-nginx and Traefik ([reverse-proxy.md](reverse-proxy.md)). |
| PodDisruptionBudget | `minAvailable: 1` with two or more replicas. |
| HorizontalPodAutoscaler | `autoscaling.enabled`; scales in by one pod at most every two minutes. |
| NATS StatefulSet | `nats.mode: bundled`: JetStream cluster with password auth, persistent volumes, lame-duck shutdown, PDB, network policy (clients: Hangar pods only). Hangar pods wait for it with an init container. |
| ServiceMonitor | Prometheus Operator scraping `/metrics` on every pod. |
| NetworkPolicy | Ingress to Hangar only from `networkPolicy.ingressFrom` (any source when empty). Optional egress restriction (`networkPolicy.egress`): DNS, NATS and the rules you list. Proxy repositories need their remotes or the corporate proxy in that list. |

## Choosing modes

| Value | Options |
|---|---|
| `storage.kind` | `s3` (recommended). `fs` with a PersistentVolumeClaim: `ReadWriteOnce` for one replica (the Deployment switches to `Recreate`), `ReadWriteMany` for more. The claim is kept on uninstall. |
| `storage.s3` credentials | `existingSecret` / `accessKeyID`+`secretAccessKey`, or none: the AWS default chain (IRSA, EKS Pod Identity) with `serviceAccount.annotations`. |
| `nats.mode` | `bundled` (default), `external` (`nats.external.url` or `existingSecret`, JetStream required), `embedded` (one replica only). |
| `migrations.mode` | `job` (default), `onStart` (one replica only), `none` (you run `hangar migrate up`). |

## GitOps (Argo CD, Flux)

Tools that render the chart offline cannot read existing Secrets, so values
generated by the chart would change on every render. Provide all of them:
`secretKey.existingSecret` (or `value`), `dockerTokenKey.existingSecret`,
`nats.bundled.existingSecret` (letters and digits only), plus
`database.existingSecret` and `storage.s3.existingSecret`. Argo CD runs the
migration hook as a `PreSync` hook.

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
policies; autoscaling with external NATS and IRSA; single node with a volume
and embedded NATS; two replicas on ReadWriteMany). Plain `helm lint` without
values fails on purpose: `config.server.base_url` and a database are
required.
