CLUSTER_NAME ?= log-collectors-bench
KUBE_CONTEXT = kind-$(CLUSTER_NAME)

KUBECTL = kubectl --context $(KUBE_CONTEXT)
HELM    = helm --kube-context $(KUBE_CONTEXT)

bench-up-all: bench-up-monitoring bench-up-collectors bench-up-generator

bench-down-all:
	kind delete cluster --name $(CLUSTER_NAME)

bench-up-monitoring: create-cluster update-helm-repos build-log-verifier
	$(HELM) upgrade --install --wait vmo vm/victoria-metrics-operator --namespace monitoring --create-namespace
	$(HELM) upgrade --install --wait vms vm/victoria-metrics-k8s-stack --namespace monitoring --values ./values/vm-metrics-stack.yml

	$(KUBECTL) apply -f ./grafana/configmap.yml
	$(KUBECTL) apply -f ./log-verifier/manifests.yml

VLS_HOST ?= log-verifier.monitoring.svc.cluster.local.
VLS_PORT ?= 8080

set-endpoint:
	grep -rl "log-verifier.monitoring.svc.cluster.local." --include "*.yml" ./ \
      | xargs sed -i 's|log-verifier.monitoring.svc.cluster.local.|$(VLS_HOST)|g'
	grep -rl "$(VLS_HOST)" --include "*.yml" ./ \
      | xargs sed -i 's|8080|$(VLS_PORT)|g'

bench-up-collectors: create-cluster update-helm-repos bench-up-vlagent bench-up-vector bench-up-promtail bench-up-alloy bench-up-grafana-agent bench-up-fluent-bit bench-up-opentelemetry-collector bench-up-filebeat bench-up-fluentd
bench-down-collectors: bench-down-vlagent bench-down-vector bench-down-promtail bench-down-alloy bench-down-grafana-agent bench-down-fluent-bit bench-down-opentelemetry-collector bench-down-filebeat bench-down-fluentd

LOGS_PER_SECOND ?= 10
RAMP_UP ?= true
RAMP_UP_STEP ?= 5
RAMP_UP_STEP_INTERVAL ?= 1s
GENERATOR_REPLICAS ?= 10

# Deploy log-generator with the current LOGS_PER_SECOND / RAMP_UP settings.
#
# By default RAMP_UP=true, which makes log-generator increase the produced
# log rate continuously by RAMP_UP_STEP every RAMP_UP_STEP_INTERVAL.
bench-up-generator: build-log-generator
	$(KUBECTL) create namespace log-generator --dry-run=client -o yaml | $(KUBECTL) apply -f -

	LOGS_PER_SECOND=$(LOGS_PER_SECOND) \
	RAMP_UP=$(RAMP_UP) \
	RAMP_UP_STEP=$(RAMP_UP_STEP) \
	RAMP_UP_STEP_INTERVAL=$(RAMP_UP_STEP_INTERVAL) \
	GENERATOR_REPLICAS=$(GENERATOR_REPLICAS) \
	envsubst < log-generator/deployment.yml | $(KUBECTL) apply -f -

bench-down-generator:
	$(KUBECTL) scale -n log-generator deploy/log-generator --replicas 0

bench-up-vlagent:
	$(HELM) upgrade --install --wait --create-namespace vlagent vm/victoria-logs-collector --version 0.2.14 --namespace collectors --values ./values/vlagent.yml

bench-down-vlagent:
	$(HELM) uninstall vlagent --namespace collectors --ignore-not-found

bench-up-vector:
	$(HELM) upgrade --install --wait --create-namespace vector vector/vector --version 0.50.0 --namespace collectors --values ./values/vector.yml

bench-down-vector:
	$(HELM) uninstall vector --namespace collectors --ignore-not-found

bench-up-promtail:
	# Do not use --wait here, since promtail requires processing at least 1 log entry to be ready
	$(HELM) upgrade --install --create-namespace promtail grafana/promtail --version 6.17.1 --namespace collectors --values ./values/promtail.yml

bench-down-promtail:
	$(HELM) uninstall promtail --namespace collectors --ignore-not-found

bench-up-alloy:
	$(HELM) upgrade --install --wait --create-namespace alloy grafana/alloy --version 1.6.1 --namespace collectors --values ./values/alloy.yml

bench-down-alloy:
	$(HELM) uninstall alloy --namespace collectors --ignore-not-found

bench-up-grafana-agent:
	$(HELM) upgrade --install --wait --create-namespace grafana-agent grafana/grafana-agent --version 0.44.2 --namespace collectors --values ./values/grafana-agent.yml

bench-down-grafana-agent:
	$(HELM) uninstall grafana-agent --namespace collectors --ignore-not-found

bench-up-fluent-bit:
	$(HELM) upgrade --install --wait --create-namespace fluent-bit fluent/fluent-bit --version 0.56.0 --namespace collectors --values ./values/fluent-bit.yml

bench-down-fluent-bit:
	$(HELM) uninstall fluent-bit --namespace collectors --ignore-not-found

bench-up-opentelemetry-collector:
	$(HELM) upgrade --install --wait --create-namespace opentelemetry-collector open-telemetry/opentelemetry-collector --version 0.146.1 --namespace collectors --values ./values/opentelemetry-collector.yml

bench-down-opentelemetry-collector:
	$(HELM) uninstall opentelemetry-collector --namespace collectors --ignore-not-found

bench-up-filebeat:
	$(HELM) upgrade --install --wait --create-namespace filebeat elastic/filebeat --version 8.5.1 --set imageTag=9.3.1 --namespace collectors --values ./values/filebeat.yml

bench-down-filebeat:
	$(HELM) uninstall filebeat --namespace collectors --ignore-not-found

bench-up-fluentd:
	$(HELM) upgrade --install --wait --create-namespace fluentd fluent/fluentd --version 0.5.3 --set image.tag=v1.19-debian-elasticsearch7-1 --namespace collectors --values ./values/fluentd.yml

bench-down-fluentd:
	$(HELM) uninstall fluentd --namespace collectors --ignore-not-found

# Create the cluster only when it is missing, so kind does not print an error
# on every run. A real failure to create it now stops the build.
create-cluster:
	@kind get clusters | grep -qx "$(CLUSTER_NAME)" \
		|| kind create cluster --config ./kind.yml --name "$(CLUSTER_NAME)"

# Helm repo commands are local and do not touch the cluster.
update-helm-repos:
	helm repo add vm https://victoriametrics.github.io/helm-charts/
	helm repo add vector https://helm.vector.dev
	helm repo add grafana https://grafana.github.io/helm-charts
	helm repo add fluent https://fluent.github.io/helm-charts
	helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts
	helm repo add elastic https://helm.elastic.co
	helm repo update

build-log-generator:
	docker build -t log-generator:latest -f ./log-generator/Dockerfile .
	kind load docker-image --name $(CLUSTER_NAME) log-generator:latest

build-log-verifier:
	docker build -t log-verifier:latest -f ./log-verifier/Dockerfile .
	kind load docker-image --name $(CLUSTER_NAME) log-verifier:latest
