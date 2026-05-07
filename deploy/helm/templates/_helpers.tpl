{{/*
Expand the name of the chart.
*/}}
{{- define "knative-agents.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "knative-agents.fullname" -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "knative-agents.labels" -}}
app.kubernetes.io/name: {{ include "knative-agents.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "knative-agents.selectorLabels" -}}
app.kubernetes.io/name: {{ include "knative-agents.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Validate the sandbox runtime class. R-SBX-1 acceptance #2: only the
allowed hardened runtimes ship without `allowHostRuntime`. Anything else
(notably runc) must be explicitly opted into.
*/}}
{{- define "knative-agents.validateSandbox" -}}
{{- $rc := .Values.sandbox.runtimeClass -}}
{{- $hardened := list "kata-fc" "kata-qemu" "kata-clh" "kata-cc-isolation" "kata" "gvisor" -}}
{{- if and (not (has $rc $hardened)) (not .Values.sandbox.allowHostRuntime) -}}
{{- fail (printf "knative-agents: sandbox.runtimeClass=%q is not a hardened runtime; set sandbox.allowHostRuntime=true to override (R-SBX-1). Hardened set: %s" $rc (join " " $hardened)) -}}
{{- end -}}
{{- end -}}
