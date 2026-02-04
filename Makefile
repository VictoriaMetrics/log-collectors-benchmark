VLS_HOST ?= vls-victoria-logs-single-server-0.vls-victoria-logs-single-server.monitoring.svc.cluster.local.
VLS_PORT ?= 9428

set-endpoint:
	grep -rl "vls-victoria-logs-single-server-0.vls-victoria-logs-single-server.monitoring.svc.cluster.local." --include "*.yml" ./ \
      | xargs sed -i 's|vls-victoria-logs-single-server-0.vls-victoria-logs-single-server.monitoring.svc.cluster.local.|$(VLS_HOST)|g'
	grep -rl "$(VLS_HOST)" --include "*.yml" ./ \
      | xargs sed -i 's|9428|$(VLS_PORT)|g'

bench-up-collectors: create-cluster update-helm-repos bench-up-vlagent bench-up-vector bench-up-promtail bench-up-alloy bench-up-grafana-agent bench-up-fluent-bit bench-up-otel-collector bench-up-filebeat bench-up-fluentd

bench-up-generator: build-log-generator
	kubectl create namespace log-generator || true
	kubectl apply -f ./log-generator/deployment.yml

bench-up-monitoring: create-cluster update-helm-repos build-log-verifier
	helm upgrade --install --wait vmo vm/victoria-metrics-operator --namespace monitoring --create-namespace
	helm upgrade --install --wait vms vm/victoria-metrics-k8s-stack --namespace monitoring --values ./values/vm-metrics-stack.yml

	helm upgrade --install --wait vls vm/victoria-logs-single --namespace monitoring

	kubectl apply -f ./grafana/configmap.yml
	kubectl apply -f ./log-verifier/deployment.yml
	kubectl apply -f ./log-verifier/vmscrape.yml

bench-down-all:
	kind delete cluster --name log-collectors-bench

bench-up-vlagent:
	# Do not pin vlagent Helm chart version because we control it and won't break backward compatibility.
	helm upgrade --install --wait --create-namespace vlagent vm/victoria-logs-collector --namespace collectors --values ./values/vlagent.yml

bench-down-vlagent:
	helm uninstall vlagent

bench-up-vector:
	helm upgrade --install --wait --create-namespace vector vector/vector --version 0.50.0 --namespace collectors --values ./values/vector.yml

bench-down-vector:
	helm uninstall vector

bench-up-promtail:
	# Do not use --wait here, since promtail requires processing at least 1 log entry to be ready
	helm upgrade --install --create-namespace promtail grafana/promtail --version 6.17.1 --namespace collectors --values ./values/promtail.yml

bench-down-promtail:
	helm uninstall promtail

bench-up-alloy:
	helm upgrade --install --wait --create-namespace alloy grafana/alloy --version 1.6.1 --namespace collectors --values ./values/alloy.yml

bench-down-alloy:
	helm uninstall alloy

bench-up-grafana-agent:
	helm upgrade --install --wait --create-namespace grafana-agent grafana/grafana-agent --version 0.44.2 --namespace collectors --values ./values/grafana-agent.yml

bench-down-grafana-agent:
	helm uninstall grafana-agent

bench-up-fluent-bit:
	helm upgrade --install --wait --create-namespace fluent-bit fluent/fluent-bit --version 0.56.0 --namespace collectors --values ./values/fluent-bit.yml

bench-down-fluent-bit:
	helm uninstall fluent-bit

bench-up-otel-collector:
	helm upgrade --install --wait --create-namespace opentelemetry-collector open-telemetry/opentelemetry-collector --version 0.146.1 --namespace collectors --values ./values/opentelemetry-collector.yml

bench-down-otel-collector:
	helm uninstall otel-collector

bench-up-filebeat:
	helm upgrade --install --wait --create-namespace filebeat elastic/filebeat --version 8.5.1 --set imageTag=9.3.1 --namespace collectors --values ./values/filebeat.yml

bench-down-filebeat:
	helm uninstall filebeat

bench-up-fluentd:
	helm upgrade --install --wait --create-namespace fluentd fluent/fluentd --version 0.5.3 --set image.tag=v1.19-debian-elasticsearch7-1 --namespace collectors --values ./values/fluentd.yml

bench-down-fluentd:
	helm uninstall fluentd

create-cluster:
	kind --version && (kind create cluster --config ./kind.yml --name log-collectors-bench || true)

update-helm-repos:
	helm repo add vm https://victoriametrics.github.io/helm-charts/
	helm repo add vector https://helm.vector.dev
	helm repo add grafana https://grafana.github.io/helm-charts
	helm repo add fluent https://fluent.github.io/helm-charts
	helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts
	helm repo add elastic https://helm.elastic.co
	helm repo update

build-log-generator:
	docker build -t log-generator:latest ./log-generator
	kind load docker-image --name log-collectors-bench log-generator:latest

build-log-verifier:
	docker build -t log-verifier:latest ./log-verifier
	kind load docker-image --name log-collectors-bench log-verifier:latest
