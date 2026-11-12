{{- define "olr.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "olr.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "olr.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "olr.selectorLabels" -}}
app.kubernetes.io/name: {{ include "olr.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "olr.labels" -}}
{{ include "olr.selectorLabels" . }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "olr.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}
{{- end -}}

{{- define "olr.secretName" -}}
{{- default (printf "%s-credentials" (include "olr.fullname" . | trunc 48 | trimSuffix "-")) .Values.existingSecret -}}
{{- end -}}

{{- define "olr.componentName" -}}
{{- printf "%s-%s" (include "olr.fullname" .root | trunc 48 | trimSuffix "-") .component -}}
{{- end -}}

{{/* Separate app names preserve the original router selector on upgrades. */}}
{{- define "olr.componentSelector" -}}
app.kubernetes.io/name: {{ printf "%s-%s" (include "olr.name" .root | trunc 48 | trimSuffix "-") .component }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* Prefer an explicit initial value, otherwise retain the installed credential. */}}
{{- define "olr.credential" -}}
{{- if .value -}}
{{- .value | b64enc -}}
{{- else if and .existing (hasKey .existing .key) -}}
{{- index .existing .key -}}
{{- else if eq .key "OLR_API_KEY_ENCRYPTION_KEY" -}}
{{- randBytes 32 | b64enc -}}
{{- else -}}
{{- randAlphaNum 32 | b64enc -}}
{{- end -}}
{{- end -}}
