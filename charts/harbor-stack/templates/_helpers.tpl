{{/*
Expand the name of the chart.
*/}}
{{- define "harbor-stack.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "harbor-stack.fullname" -}}
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
Common labels
*/}}
{{- define "harbor-stack.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "harbor-stack.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Generate the admin password secret name.
*/}}
{{- define "harbor-stack.adminPasswordSecret" -}}
harbor-admin-password
{{- end }}

{{/*
Database password secret name.
*/}}
{{- define "harbor-stack.databasePasswordSecret" -}}
{{- printf "%s-database-password" (include "harbor-stack.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Redis password secret name.
*/}}
{{- define "harbor-stack.redisPasswordSecret" -}}
{{- printf "%s-redis-password" (include "harbor-stack.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
