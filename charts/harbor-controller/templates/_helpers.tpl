{{/*
Expand the name of the chart.
*/}}
{{- define "harbor-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "harbor-controller.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "harbor-controller.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "harbor-controller.labels" -}}
helm.sh/chart: {{ include "harbor-controller.chart" . }}
{{ include "harbor-controller.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "harbor-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "harbor-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the ServiceAccount to use
*/}}
{{- define "harbor-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "harbor-controller.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Construct a namespace-qualified registry host for the dockerconfigjson Secret.
If the configured registryHost is a short service name (no dots and no port
separator that looks like a hostname), qualify it with the release namespace
so it resolves from any target namespace.
Examples:
  "harbor:80"           → "harbor.<namespace>.svc:80"
  "harbor:443"          → "harbor.<namespace>.svc:443"
  "harbor.svc:80"       → "harbor.svc:80"         (unchanged)
  "registry.sealos.io"  → "registry.sealos.io"    (unchanged)
*/}}
{{- define "harbor-controller.registryHost" -}}
{{- $host := .Values.harbor.registryHost | default "registry.sealos.io" -}}
{{- $serviceName := splitList ":" $host | first -}}
{{- $port := splitList ":" $host | last -}}
{{- /* If the service name has no dots, it's a short name → qualify */ -}}
{{- if not (contains "." $serviceName) -}}
{{- printf "%s.%s.svc:%s" $serviceName .Release.Namespace $port -}}
{{- else -}}
{{- $host -}}
{{- end -}}
{{- end -}}
