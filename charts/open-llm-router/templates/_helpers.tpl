{{- define "olr.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Inject the optional demo model without inherited environment references. */}}
{{- define "olr.config" -}}
{{- $config := deepCopy .Values.config -}}
{{- $models := $config.models | default dict -}}
{{- if hasKey $models "chat-model" -}}
{{- $chat := index $models "chat-model" -}}
{{- $legacyDemo := and (eq (len ($chat.pool | default list)) 1) (eq (index $chat.pool 0).url "${OLR_DEMO_BACKEND_URL}") -}}
{{- if or (empty $chat.pool) $legacyDemo -}}
{{- if .Values.demoBackend.enabled -}}
{{- if $legacyDemo -}}
{{- $_ := set (index $chat.pool 0) "url" (printf "http://%s:8000" (include "olr.componentName" (dict "root" . "component" "demo"))) -}}
{{- else -}}
{{- $_ := set $chat "pool" (list (dict "url" (printf "http://%s:8000" (include "olr.componentName" (dict "root" . "component" "demo"))) "health_path" "/health" "weight" 1 "max_inflight" 4)) -}}
{{- end -}}
{{- else -}}
{{- $_ := unset $models "chat-model" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $_ := set $config "models" $models -}}
{{- toYaml $config -}}
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
{{- default (printf "%s-credentials" (include "olr.resourcePrefix" .)) .Values.existingSecret -}}
{{- end -}}

{{- define "olr.componentName" -}}
{{- printf "%s-%s" (include "olr.resourcePrefix" .root) .component -}}
{{- end -}}

{{/* Retain release identity when reserving space for component suffixes. */}}
{{- define "olr.resourcePrefix" -}}
{{- $name := include "olr.fullname" . -}}
{{- if gt (len $name) 48 -}}
{{- printf "%s-%s" ($name | trunc 39 | trimSuffix "-") ($name | sha256sum | trunc 8) -}}
{{- else -}}
{{- $name -}}
{{- end -}}
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
{{- else -}}
{{- randAlphaNum 32 | b64enc -}}
{{- end -}}
{{- end -}}
