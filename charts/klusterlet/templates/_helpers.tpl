{{/* Generate kubeconfig template */}}
{{- define "mychart.kubeconfig" }}
apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: {{ .CA }}
    server: {{ .APIServer }}
  name: hub
contexts:
- context:
    cluster: hub
    user: hub
  name: hub
current-context: hub
kind: Config
preferences: {}
users:
- name: hub
  user:
    token: {{ .Token }}
{{- end }}