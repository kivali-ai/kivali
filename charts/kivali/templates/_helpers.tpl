{{/* The server hard-codes every object name below; refuse anything else. */}}
{{- define "kivali.validate" -}}
{{- /* `helm lint` always renders under the release name "test-release"; refusing it would make lint skip every template. Exactly that name is let through (lint renders nothing into a cluster); every other name still fails. */ -}}
{{- if not (has .Release.Name (list "kivali" "test-release")) -}}
{{- fail (printf "the kivali chart must be installed with release name \"kivali\" (got %q): the server hard-codes the names Deployment kivali, Services kivali and kivali-egress, PVC kivali-data, Secret kivali-secrets and ServiceAccount kivali. Run: helm install kivali <chart> -n <namespace>" .Release.Name) -}}
{{- end -}}
{{- if not (has .Values.kivaliEnv (list "dev" "prod")) -}}
{{- fail (printf "kivaliEnv must be \"dev\" or \"prod\" (got %q)" .Values.kivaliEnv) -}}
{{- end -}}
{{- if and .Values.devMode (eq .Values.kivaliEnv "prod") -}}
{{- fail "devMode: true with kivaliEnv: prod is refused by the server (an unauthenticated production server); set kivaliEnv: dev" -}}
{{- end -}}
{{- end -}}

{{- define "kivali.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "kivali.egressImage" -}}
{{ .Values.egress.image.repository }}:{{ .Values.egress.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "kivali.labels" -}}
app: kivali
app.kubernetes.io/name: kivali
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end -}}
