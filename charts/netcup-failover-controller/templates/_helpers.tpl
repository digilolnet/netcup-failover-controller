{{- define "netcup-failover-controller.name" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "netcup-failover-controller.image" -}}
{{- $tag := .Values.image.tag | default (.Chart.AppVersion | trimPrefix "v") }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{- define "netcup-failover-controller.labels" -}}
app.kubernetes.io/name: netcup-failover-controller
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "netcup-failover-controller.selectorLabels" -}}
app.kubernetes.io/name: netcup-failover-controller
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
