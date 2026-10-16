{{- define "murmur-hub.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "murmur-hub.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "murmur-hub.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "murmur-hub.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "murmur-hub.selectorLabels" -}}
app.kubernetes.io/name: {{ include "murmur-hub.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "murmur-hub.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "murmur-hub.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The scheme of a store or bus URL: "postgres" for postgres://..., "" when the value is empty. */}}
{{- define "murmur-hub.scheme" -}}
{{- if contains "://" . }}{{ first (splitList "://" .) }}{{ end }}
{{- end }}

{{/* "true" when the hub keeps its data in sqlite: the image's default store, or a sqlite:// URL. */}}
{{- define "murmur-hub.sqlite" -}}
{{- $scheme := include "murmur-hub.scheme" .Values.store }}
{{- if or (not .Values.store) (eq $scheme "sqlite") }}true{{ end }}
{{- end }}

{{/* "true" when a volume claim backs /data: sqlite, persistence on, one replica. */}}
{{- define "murmur-hub.pvc" -}}
{{- if and (include "murmur-hub.sqlite" .) .Values.persistence.enabled (eq (int .Values.replicaCount) 1) }}true{{ end }}
{{- end }}

{{/* "true" when replicas can share events: any bus but local://. */}}
{{- define "murmur-hub.networkedBus" -}}
{{- $scheme := include "murmur-hub.scheme" .Values.bus }}
{{- if has $scheme (list "postgres" "postgresql" "redis" "rediss") }}true{{ end }}
{{- end }}

{{/*
"true" when an old and a new pod may run side by side during a rolling update: they share a networked store and a
networked bus. Otherwise the chart uses the Recreate strategy.
*/}}
{{- define "murmur-hub.rolling" -}}
{{- $store := include "murmur-hub.scheme" .Values.store }}
{{- if and (include "murmur-hub.networkedBus" .) (has $store (list "postgres" "postgresql" "firestore")) }}true{{ end }}
{{- end }}

{{/*
More than one replica needs a store they share and a bus that carries events between them. sqlite and memory://
live inside one pod, and local:// stays inside one process: replicas using them would each see part of the team.
*/}}
{{- define "murmur-hub.validate" -}}
{{- if gt (int .Values.replicaCount) 1 }}
{{- $store := include "murmur-hub.scheme" .Values.store }}
{{- if not (has $store (list "postgres" "postgresql" "firestore")) }}
{{- fail (printf "replicaCount is %d: set store to a postgres:// or firestore:// URL that every replica shares (sqlite and memory:// live inside one pod)" (int .Values.replicaCount)) }}
{{- end }}
{{- if not (include "murmur-hub.networkedBus" .) }}
{{- fail (printf "replicaCount is %d: set bus to a postgres://, redis:// or rediss:// URL so replicas share events (local:// stays inside one pod)" (int .Values.replicaCount)) }}
{{- end }}
{{- end }}
{{- end }}
