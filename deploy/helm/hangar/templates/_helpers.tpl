{{/* Names */}}
{{- define "hangar.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "hangar.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "hangar.natsName" -}}
{{- printf "%s-nats" (include "hangar.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "hangar.trivyName" -}}
{{- printf "%s-trivy" (include "hangar.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "hangar.scannerName" -}}
{{- printf "%s-scanner" (include "hangar.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "hangar.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "hangar.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* Labels */}}
{{- define "hangar.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "hangar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | trunc 63 | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: hangar
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "hangar.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hangar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "hangar.natsSelectorLabels" -}}
app.kubernetes.io/name: {{ include "hangar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: nats
{{- end -}}

{{- define "hangar.trivySelectorLabels" -}}
app.kubernetes.io/name: {{ include "hangar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: trivy
{{- end -}}

{{- define "hangar.scannerSelectorLabels" -}}
app.kubernetes.io/name: {{ include "hangar.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: scanner
{{- end -}}

{{/* Image */}}
{{- define "hangar.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}

{{/* Scaling */}}
{{/* Replicas of Hangar's JetStream streams: nats.streamReplicas, or automatic. */}}
{{- define "hangar.streamReplicas" -}}
{{- $sr := int .Values.nats.streamReplicas -}}
{{- if gt $sr 0 -}}{{ $sr }}
{{- else if eq .Values.nats.mode "bundled" -}}{{ int .Values.nats.bundled.replicas }}
{{- else -}}1
{{- end -}}
{{- end -}}

{{- define "hangar.multiReplica" -}}
{{- if or .Values.autoscaling.enabled (gt (int .Values.replicaCount) 1) -}}true{{- end -}}
{{- end -}}

{{/* Secret names: the user's existing Secret or the chart's own. */}}
{{- define "hangar.secretName" -}}{{ include "hangar.fullname" . }}{{- end -}}
{{- define "hangar.databaseSecret" -}}{{ .Values.database.existingSecret | default (include "hangar.secretName" .) }}{{- end -}}
{{- define "hangar.s3Secret" -}}{{ .Values.storage.s3.existingSecret | default (include "hangar.secretName" .) }}{{- end -}}
{{- define "hangar.secretKeySecret" -}}{{ .Values.secretKey.existingSecret | default (include "hangar.secretName" .) }}{{- end -}}
{{- define "hangar.dockerTokenSecret" -}}{{ .Values.dockerTokenKey.existingSecret | default (include "hangar.secretName" .) }}{{- end -}}
{{- define "hangar.bootstrapSecret" -}}{{ .Values.bootstrapAdmin.existingSecret | default (include "hangar.secretName" .) }}{{- end -}}
{{- define "hangar.proxyPasswordSecret" -}}{{ .Values.httpProxyPassword.existingSecret | default (include "hangar.secretName" .) }}{{- end -}}
{{- define "hangar.natsExternalSecret" -}}{{ .Values.nats.external.existingSecret | default (include "hangar.secretName" .) }}{{- end -}}
{{- define "hangar.scannerSecret" -}}{{ .Values.imageScanning.adapterToken.existingSecret | default (include "hangar.scannerName" .) }}{{- end -}}
{{- define "hangar.natsPasswordSecret" -}}{{ .Values.nats.bundled.existingSecret | default (include "hangar.natsName" .) }}{{- end -}}

{{- define "hangar.s3StaticCredentials" -}}
{{- if or .Values.storage.s3.existingSecret .Values.storage.s3.accessKeyID -}}true{{- end -}}
{{- end -}}

{{- define "hangar.bootstrapEnabled" -}}
{{- if or .Values.bootstrapAdmin.existingSecret .Values.bootstrapAdmin.username -}}true{{- end -}}
{{- end -}}

{{/*
A generated secret value that survives upgrades: the value already stored in
Secret $name under $key (helm lookup), or a new random one.
Usage: include "hangar.persistentRandom" (list $ "secret-name" "key" ["bytes"])
New values are 48 letters and digits, or with "bytes" 32 random bytes in
base64 (the format security.secret_key expects).
*/}}
{{- define "hangar.persistentRandom" -}}
{{- $root := index . 0 -}}
{{- $existing := lookup "v1" "Secret" $root.Release.Namespace (index . 1) -}}
{{- if and $existing $existing.data (hasKey $existing.data (index . 2)) -}}
{{- index $existing.data (index . 2) | b64dec -}}
{{- else if and (gt (len .) 3) (eq (index . 3) "bytes") -}}
{{- randBytes 32 -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}

{{/*
Checksum of the secret settings supplied in values, so pods restart when they
change. Generated values are left out on purpose: they would differ on every
offline render.
*/}}
{{- define "hangar.secretValuesChecksum" -}}
{{- $v := list .Values.database .Values.storage.s3 .Values.secretKey .Values.dockerTokenKey .Values.bootstrapAdmin .Values.httpProxyPassword .Values.nats.external .Values.nats.bundled.password .Values.nats.bundled.existingSecret .Values.imageScanning.adapterToken -}}
{{- toJson $v | sha256sum -}}
{{- end -}}

{{/* hangar.yaml: config values plus the keys the chart manages. */}}
{{- define "hangar.configYaml" -}}
{{- $cfg := deepCopy .Values.config -}}
{{- $server := (get $cfg "server") | default (dict) -}}
{{- $_ := set $server "addr" ":8080" -}}
{{- $_ := set $cfg "server" $server -}}
{{- $db := (get $cfg "database") | default (dict) -}}
{{- $_ := unset $db "url" -}}
{{- $_ := set $db "migrate_on_start" (eq .Values.migrations.mode "onStart") -}}
{{- $_ := set $cfg "database" $db -}}
{{- $storage := (get $cfg "storage") | default (dict) -}}
{{- $_ := set $storage "kind" .Values.storage.kind -}}
{{- $_ := unset $storage "fs" -}}
{{- $_ := unset $storage "s3" -}}
{{- if eq .Values.storage.kind "fs" -}}
{{- $_ := set $storage "fs" (dict "path" "/var/lib/hangar/blobs") -}}
{{- else -}}
{{- $s3 := dict "endpoint" .Values.storage.s3.endpoint "region" .Values.storage.s3.region "bucket" .Values.storage.s3.bucket "prefix" .Values.storage.s3.prefix "use_path_style" .Values.storage.s3.usePathStyle -}}
{{- if .Values.storage.s3.partSize -}}{{- $_ := set $s3 "part_size" (int64 .Values.storage.s3.partSize) -}}{{- end -}}
{{- if .Values.storage.s3.caFile -}}{{- $_ := set $s3 "ca_file" .Values.storage.s3.caFile -}}{{- end -}}
{{- $_ := set $storage "s3" $s3 -}}
{{- end -}}
{{- $_ := set $cfg "storage" $storage -}}
{{- if eq .Values.nats.mode "embedded" -}}
{{- $_ := set $cfg "nats" (dict "embedded" true "data_dir" "/var/lib/hangar/nats" "replicas" 1) -}}
{{- else -}}
{{- /* The URL (with credentials) comes from HANGAR_NATS_URL. */ -}}
{{- $_ := set $cfg "nats" (dict "embedded" false "replicas" (include "hangar.streamReplicas" . | int)) -}}
{{- end -}}
{{- $metrics := (get $cfg "metrics") | default (dict) -}}
{{- range $k := list "bearer_token" "bearer_token_file" "username" "password" "password_file" -}}{{- $_ := unset $metrics $k -}}{{- end -}}
{{- $_ := set $metrics "auth" .Values.metrics.auth.mode -}}
{{- $_ := set $cfg "metrics" $metrics -}}
{{- $_ := unset $cfg "docker" -}}
{{- $_ := unset $cfg "bootstrap" -}}
{{- $security := (get $cfg "security") | default (dict) -}}
{{- $_ := unset $security "secret_key" -}}
{{- $_ := unset $security "previous_secret_keys" -}}
{{- if $security -}}{{- $_ := set $cfg "security" $security -}}{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}

{{/* Fails the render with an actionable message on unsupported combinations. */}}
{{- define "hangar.validate" -}}
{{- $multi := include "hangar.multiReplica" . -}}
{{- $base := dig "server" "base_url" "" .Values.config | toString -}}
{{- if not (regexMatch "^https?://[^/]+" $base) -}}
{{- fail "config.server.base_url is required: the external URL clients use, e.g. https://hangar.example.com" -}}
{{- end -}}
{{- if not (or .Values.database.url .Values.database.existingSecret) -}}
{{- fail "set database.url or database.existingSecret (PostgreSQL connection URL)" -}}
{{- end -}}
{{- range $path := list "database.url" "storage.s3.access_key_id" "storage.s3.secret_access_key" "storage.s3.session_token" "http.proxy_password" "docker.token_key" "bootstrap.admin_password" "security.secret_key" "security.previous_secret_keys" "metrics.bearer_token" "metrics.password" "scanner.adapter_token" "scanner.trivy_token" -}}
{{- $parts := splitList "." $path -}}
{{- $v := $.Values.config -}}
{{- range $parts -}}{{- if kindIs "map" $v -}}{{- $v = get $v . -}}{{- else -}}{{- $v = "" -}}{{- end -}}{{- end -}}
{{- if $v -}}
{{- fail (printf "config.%s is a secret: set it through the chart's secret values (database, storage.s3, httpProxyPassword, dockerTokenKey, bootstrapAdmin, secretKey, metrics.auth) so it never lands in the ConfigMap" $path) -}}
{{- end -}}
{{- end -}}
{{- if not (has .Values.storage.kind (list "s3" "fs")) -}}
{{- fail "storage.kind must be s3 or fs" -}}
{{- end -}}
{{- if and (eq .Values.storage.kind "s3") (not .Values.storage.s3.bucket) -}}
{{- fail "storage.s3.bucket is required for storage.kind=s3" -}}
{{- end -}}
{{- if and (not .Values.storage.s3.existingSecret) (ne (empty .Values.storage.s3.accessKeyID) (empty .Values.storage.s3.secretAccessKey)) -}}
{{- fail "storage.s3.accessKeyID and storage.s3.secretAccessKey must be set together" -}}
{{- end -}}
{{- if and (eq .Values.storage.kind "fs") $multi (not (has "ReadWriteMany" .Values.storage.fs.persistence.accessModes)) -}}
{{- fail "storage.kind=fs with more than one replica (or autoscaling) needs storage.fs.persistence.accessModes [ReadWriteMany]; or use S3" -}}
{{- end -}}
{{- if not (has .Values.nats.mode (list "bundled" "external" "embedded")) -}}
{{- fail "nats.mode must be bundled, external or embedded" -}}
{{- end -}}
{{- if and (eq .Values.nats.mode "embedded") $multi -}}
{{- fail "nats.mode=embedded runs NATS inside one process: use replicaCount 1 without autoscaling, or nats.mode=bundled/external" -}}
{{- end -}}
{{- if and (eq .Values.nats.mode "external") (not (or .Values.nats.external.url .Values.nats.external.existingSecret)) -}}
{{- fail "nats.mode=external needs nats.external.url or nats.external.existingSecret" -}}
{{- end -}}
{{- if and (eq .Values.nats.mode "bundled") .Values.nats.bundled.password (not (regexMatch "^[A-Za-z0-9]+$" .Values.nats.bundled.password)) -}}
{{- fail "nats.bundled.password may contain only letters and digits (it is embedded in a URL)" -}}
{{- end -}}
{{- if and .Values.imageScanning.enabled (dig "scanner" "trivy_url" "" .Values.config) -}}
{{- fail "imageScanning.enabled wires config.scanner itself: leave config.scanner.trivy_url empty" -}}
{{- end -}}
{{- if and .Values.imageScanning.enabled .Values.imageScanning.adapterToken.value (lt (len .Values.imageScanning.adapterToken.value) 16) -}}
{{- fail "imageScanning.adapterToken.value must be at least 16 characters" -}}
{{- end -}}
{{- if not (has .Values.migrations.mode (list "job" "onStart" "none")) -}}
{{- fail "migrations.mode must be job, onStart or none" -}}
{{- end -}}
{{- $sr := int .Values.nats.streamReplicas -}}
{{- if or (lt $sr 0) (gt $sr 5) -}}
{{- fail "nats.streamReplicas must be 0 (automatic) or 1 to 5" -}}
{{- end -}}
{{- if and (eq .Values.nats.mode "embedded") (gt $sr 1) -}}
{{- fail "nats.streamReplicas > 1 needs a NATS cluster: embedded NATS is a single server" -}}
{{- end -}}
{{- if and (eq .Values.nats.mode "bundled") (gt $sr (int .Values.nats.bundled.replicas)) -}}
{{- fail (printf "nats.streamReplicas %d exceeds nats.bundled.replicas %d: a stream cannot have more replicas than NATS servers" $sr (int .Values.nats.bundled.replicas)) -}}
{{- end -}}
{{- if not (has .Values.metrics.auth.mode (list "none" "bearer" "basic")) -}}
{{- fail "metrics.auth.mode must be none, bearer or basic" -}}
{{- end -}}
{{- if and (ne .Values.metrics.auth.mode "none") (not .Values.metrics.auth.existingSecret) -}}
{{- fail (printf "metrics.auth.mode=%s needs metrics.auth.existingSecret holding the credentials" .Values.metrics.auth.mode) -}}
{{- end -}}
{{- if and (not .Values.bootstrapAdmin.existingSecret) (ne (empty .Values.bootstrapAdmin.username) (empty .Values.bootstrapAdmin.password)) -}}
{{- fail "bootstrapAdmin.username and bootstrapAdmin.password must be set together" -}}
{{- end -}}
{{- end -}}
