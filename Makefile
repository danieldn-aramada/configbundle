# Registry and per-component versioning.
# Component tag namespaces:
#   controller/v*    → cb-controller
#   bundler/v*       → cb-bundler
#   serverconfig/v*  → sc-controller
#   backupconfig/v*  → bc-controller
# Tag with: git tag controller/v0.1.0  (or serverconfig/v0.1.0, etc.)
#
# Versions resolve from per-component git tags. Override one explicitly with
# the matching var on the trailing position:
#   make push-controller CONTROLLER_VERSION=v0.1.0
#   make push-bundler    BUNDLER_VERSION=v0.0.4
# (There is intentionally NO `VERSION` umbrella var — it silently shadowed
# the tag-derived version when exported in the shell. Tag the commit instead.)
ACR                := armadaeksatest.azurecr.io
MODULE             := github.com/armada/configbundle
CONTROLLER_VERSION   ?= $(shell (git describe --tags --match 'controller/v*' --dirty 2>/dev/null || echo "controller/v0.0.0-dev") | sed 's|^controller/||')
BUNDLER_VERSION      ?= $(shell (git describe --tags --match 'bundler/v*' --dirty 2>/dev/null || echo "bundler/v0.0.0-dev") | sed 's|^bundler/||')
SERVERCONFIG_VERSION ?= $(shell (git describe --tags --match 'serverconfig/v*' --dirty 2>/dev/null || echo "serverconfig/v0.0.0-dev") | sed 's|^serverconfig/||')
BACKUPCONFIG_VERSION ?= $(shell (git describe --tags --match 'backupconfig/v*' --dirty 2>/dev/null || echo "backupconfig/v0.0.0-dev") | sed 's|^backupconfig/||')
BUNDLER_LDFLAGS      := -ldflags "-X $(MODULE)/internal/version.Version=$(BUNDLER_VERSION)"

# Image URLs
IMG              ?= $(ACR)/configbundle-controller:$(CONTROLLER_VERSION)
BUNDLER_IMG      ?= $(ACR)/configbundle-bundler:$(BUNDLER_VERSION)
SERVERCONFIG_IMG ?= $(ACR)/serverconfig-controller:$(SERVERCONFIG_VERSION)
BACKUPCONFIG_IMG ?= $(ACR)/backupconfig-controller:$(BACKUPCONFIG_VERSION)

# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# kubectl kuberc is disabled by default for test isolation; enable with:
# - KUBECTL_KUBERC=true
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= configbundle-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: ## Set up a Kind cluster for e2e tests if it does not exist
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests against Kind (CI). Builds image and deploys controller.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

.PHONY: test-e2e-local
test-e2e-local: manifests generate fmt vet ## Run ConfigBundle e2e tests against a running local controller.
	@echo "Requires: 'make install' applied and 'make run-controller' running in another terminal."
	CONTROLLER_RUNNING=true go test -tags=e2e ./test/e2e/ -v -ginkgo.v -ginkgo.focus "ConfigBundle"

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build all manager binaries.
	go build -o bin/manager cmd/controller/main.go
	go build $(BUNDLER_LDFLAGS) -o bin/bundler cmd/bundler/main.go
	go build -o bin/serverconfig cmd/serverconfig/main.go
	go build -o bin/backupconfig cmd/backupconfig/main.go

.PHONY: up
up: ## Start minikube and install CRDs — ready for 'make run-controller'.
	@minikube status >/dev/null 2>&1 || minikube start
	@$(MAKE) install
	@echo ""
	@echo "Cluster ready. Next steps:"
	@echo "  Terminal 1: make run-controller"
	@echo "  Terminal 2: make run-bundler   (optional — only if testing bundler)"
	@echo ""
	@echo "Env vars for local testing with orb:"
	@echo "  NAMESPACE=default CB_CONTROLLER_PORT=:8095 ORB_DIVERGENCE_INTAKE_URL=http://localhost:8010/api/v1/divergence DIVERGENCE_REPORTER_ENABLED=true"

.PHONY: down
down: ## Stop minikube.
	minikube stop

.PHONY: run-controller
run-controller: ## Run the cb-controller from your host (set NAMESPACE=default for local testing).
	go run ./cmd/controller/main.go

.PHONY: run-bundler
run-bundler: ## Run the bundler service locally (BUNDLER_PORT=8020, ORBITAL_BASE_URL=http://localhost:8001).
	go run $(BUNDLER_LDFLAGS) ./cmd/bundler/main.go

.PHONY: run-serverconfig
run-serverconfig: ## Run the sc-controller locally (:8092 health, :8093 metrics; requires IDRAC_OOB_ALLOWLIST + IDRAC_FIELD_ALLOWLIST set).
	go run ./cmd/serverconfig/main.go

.PHONY: run-backupconfig
run-backupconfig: ## Run the bc-controller locally (:8094 health, :8096 metrics).
	go run ./cmd/backupconfig/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: push-controller
push-controller: ## Build and push controller image to ACR (e.g. make push-controller CONTROLLER_VERSION=v0.1.0). Requires: az acr login --name armadaeksatest
	@echo "Building $(IMG)"
	docker buildx build --platform linux/amd64 --target controller -t $(IMG) --push .

.PHONY: push-bundler
push-bundler: ## Build and push bundler image to ACR (e.g. make push-bundler BUNDLER_VERSION=v0.0.4). Requires: az acr login --name armadaeksatest
	@echo "Building $(BUNDLER_IMG)"
	docker buildx build --platform linux/amd64 --target bundler \
		--build-arg BUNDLER_VERSION=$(BUNDLER_VERSION) \
		-t $(BUNDLER_IMG) --push .

.PHONY: push-serverconfig
push-serverconfig: ## Build and push sc-controller image to ACR (e.g. make push-serverconfig SERVERCONFIG_VERSION=v0.1.0). Requires: az acr login --name armadaeksatest
	@echo "Building $(SERVERCONFIG_IMG)"
	docker buildx build --platform linux/amd64 --target serverconfig -t $(SERVERCONFIG_IMG) --push .

.PHONY: push-backupconfig
push-backupconfig: ## Build and push bc-controller image to ACR (e.g. make push-backupconfig BACKUPCONFIG_VERSION=v0.1.0). Requires: az acr login --name armadaeksatest
	@echo "Building $(BACKUPCONFIG_IMG)"
	docker buildx build --platform linux/amd64 --target backupconfig -t $(BACKUPCONFIG_IMG) --push .

.PHONY: push-all
push-all: push-controller push-bundler push-serverconfig push-backupconfig ## Build and push all 4 images to ACR (versions resolved from per-component git tags). Requires: az acr login --name armadaeksatest

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name configbundle-builder
	$(CONTAINER_TOOL) buildx use configbundle-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm configbundle-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Monitoring (local)

# Replicates the dev-main monitoring stack (kube-prometheus-stack: Prometheus,
# Alertmanager, Grafana, kube-state-metrics) on minikube so the SAME
# ServiceMonitors + PrometheusRule can be applied and alerts watched firing —
# entirely locally. Nothing pages: the bundled Alertmanager has no external
# receiver, so fired alerts are visible in its UI but dispatched nowhere.
HELM ?= helm
MONITORING_NS ?= monitoring
MONITORING_RELEASE ?= kube-prometheus-stack

.PHONY: monitoring-up
monitoring-up: ## Install kube-prometheus-stack on minikube + apply our ServiceMonitors & PrometheusRule (local alert rig — pages nobody).
	@command -v $(HELM) >/dev/null 2>&1 || { echo "helm not found — 'brew install helm' first"; exit 1; }
	$(HELM) repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
	$(HELM) repo update prometheus-community >/dev/null
	$(HELM) upgrade --install $(MONITORING_RELEASE) prometheus-community/kube-prometheus-stack \
		--namespace $(MONITORING_NS) --create-namespace --wait --timeout 10m \
		--set grafana.adminPassword=admin \
		--set kubeControllerManager.enabled=false \
		--set kubeScheduler.enabled=false \
		--set kubeEtcd.enabled=false \
		--set kubeProxy.enabled=false
	@$(KUBECTL) create namespace configbundle-system --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) apply -k config/prometheus/service-monitors
	$(KUBECTL) apply -k config/prometheus/rules
	@echo ""
	@echo "Monitoring stack up in '$(MONITORING_NS)'; our ServiceMonitors + PrometheusRule applied."
	@echo "Access (each in its own terminal):"
	@echo "  kubectl -n $(MONITORING_NS) port-forward svc/$(MONITORING_RELEASE)-grafana 3000:80         # Grafana   admin/admin (Explore runs 'up')"
	@echo "  kubectl -n $(MONITORING_NS) port-forward svc/$(MONITORING_RELEASE)-prometheus 9090:9090    # Prometheus (Status>Targets, Alerts)"
	@echo "  kubectl -n $(MONITORING_NS) port-forward svc/$(MONITORING_RELEASE)-alertmanager 9093:9093  # Alertmanager (fired alerts land here, dispatched nowhere)"
	@echo ""
	@echo "No controllers deployed yet, so *ControllerDown alerts fire after 5m (expected); 'make deploy' clears them."

.PHONY: monitoring-down
monitoring-down: ## Tear down the local monitoring stack and our monitoring objects.
	-$(KUBECTL) delete -k config/prometheus/rules --ignore-not-found
	-$(KUBECTL) delete -k config/prometheus/service-monitors --ignore-not-found
	-$(HELM) uninstall $(MONITORING_RELEASE) --namespace $(MONITORING_NS)
	-$(KUBECTL) delete namespace $(MONITORING_NS) --ignore-not-found

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.20.1

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.11.4
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
