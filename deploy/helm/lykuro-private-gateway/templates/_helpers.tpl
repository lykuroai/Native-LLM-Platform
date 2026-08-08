{{- define "pgw.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pgw.fullname" -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "pgw.labels" -}}
app.kubernetes.io/name: {{ include "pgw.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "pgw.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pgw.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
