{{- define "typeclaw-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "typeclaw-operator.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "typeclaw-operator.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "typeclaw-operator.labels" -}}
app.kubernetes.io/name: {{ include "typeclaw-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "typeclaw-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "typeclaw-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "typeclaw-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "typeclaw-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
