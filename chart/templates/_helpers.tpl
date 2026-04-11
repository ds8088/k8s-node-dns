{{/*
  Expand the name of the chart.
*/}}
{{- define "k8s-node-dns.name" -}}
  {{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
  Create a default fully qualified app name.
*/}}
{{- define "k8s-node-dns.fullname" -}}
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
{{- define "k8s-node-dns.chart" -}}
  {{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
  Common labels.
*/}}
{{- define "k8s-node-dns.labels" -}}
helm.sh/chart: {{ include "k8s-node-dns.chart" . }}
{{ include "k8s-node-dns.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
app.kubernetes.io/component: {{ .Values.componentOverride | default "k8s-node-dns" | quote }}
{{- end }}

{{/*
  Selector labels.
*/}}
{{- define "k8s-node-dns.selectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-node-dns.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
  ServiceAccount name.
*/}}
{{- define "k8s-node-dns.serviceAccountName" -}}
  {{- default (include "k8s-node-dns.fullname" .) .Values.serviceAccount.name }}
{{- end }}

{{/*
  Role and RoleBinding name for leader election.
*/}}
{{- define "k8s-node-dns.roleName" -}}
  {{- include "k8s-node-dns.fullname" . }}-leader-election
{{- end }}
