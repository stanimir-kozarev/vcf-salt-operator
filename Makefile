# Image URL to use all building/pushing image targets
IMG ?= controller:latest

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
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

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
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
KIND_CLUSTER ?= vcf-salt-operator-test-e2e

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
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) go test -tags=e2e ./test/e2e/ -v -ginkgo.v
	$(MAKE) cleanup-test-e2e

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
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

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
	- $(CONTAINER_TOOL) buildx create --name vcf-salt-operator-builder
	$(CONTAINER_TOOL) buildx use vcf-salt-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm vcf-salt-operator-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

.PHONY: build-tar
build-tar: ## Build image with Podman and save to /tmp/vcf-salt-op.tar for transfer to sandbox.
	$(CONTAINER_TOOL) build -t $(IMG) .
	$(CONTAINER_TOOL) save $(IMG) -o /tmp/vcf-salt-op.tar
	@echo "Saved to /tmp/vcf-salt-op.tar  ($(shell du -sh /tmp/vcf-salt-op.tar | cut -f1))"

.PHONY: build-installer-vcf9
build-installer-vcf9: manifests generate kustomize ## Generate minimal install bundle for VCF9 Supervisor (no metrics, no cert-manager).
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/vcf9 > dist/install-vcf9.yaml

##@ Supervisor Service (VCF9)

# Release version for the Supervisor Service bundle + Package.
VERSION          ?= 0.1.0
# Service ID — must match PackageMetadata.metadata.name in config/supervisor-service/package/.
SERVICE_ID       ?= vcf-salt-operator.salt.vcf.io
# OCI bundle published by `make supervisor-bundle`.
BUNDLE_IMG       ?= ghcr.io/stanimir-kozarev/vcf-salt-operator-bundle:$(VERSION)
SUPSVC_SRC       := config/supervisor-service
SUPSVC_BUNDLE    := $(SUPSVC_SRC)/bundle
SUPSVC_PKG_DIR   := $(SUPSVC_SRC)/package
SUPSVC_CRD_SRC   := config/crd/bases/salt.vcf.io_saltkeyconfigs.yaml
SUPSVC_CRD_DST   := $(SUPSVC_BUNDLE)/config/002-crd.yaml
SUPSVC_YAML_OUT  := dist/vcf-salt-operator-supervisorservice-$(VERSION).yaml

.PHONY: supervisor-crd-sync
supervisor-crd-sync: manifests ## Refresh the CRD baked into the Supervisor Service bundle.
	@echo "Syncing $(SUPSVC_CRD_SRC) -> $(SUPSVC_CRD_DST)"
	@{ \
		printf '%s\n' '#! This file is auto-generated by `make supervisor-crd-sync`.'; \
		printf '%s\n' '#! Source: $(SUPSVC_CRD_SRC). Do not edit by hand.'; \
		cat $(SUPSVC_CRD_SRC); \
	} > $(SUPSVC_CRD_DST)

.PHONY: supervisor-bundle
supervisor-bundle: supervisor-crd-sync ## Build and push the imgpkg bundle to BUNDLE_IMG. Requires imgpkg + kbld + ytt in PATH.
	@command -v imgpkg >/dev/null 2>&1 || { echo "imgpkg not found. Install Carvel imgpkg."; exit 1; }
	@command -v kbld   >/dev/null 2>&1 || { echo "kbld not found. Install Carvel kbld.";     exit 1; }
	@command -v ytt    >/dev/null 2>&1 || { echo "ytt not found. Install Carvel ytt.";       exit 1; }
	@# Bake the build-time image ref into the bundle so deploy-time ytt renders the same
	@# literal that kbld will later find in the (relocated) ImagesLock.
	@img_repo="$$(printf '%s\n' '$(IMG)' | sed -E 's/:[^:/]+$$//')"; \
	img_tag="$$(printf  '%s\n' '$(IMG)' | sed -E 's#.*:##')";  \
	printf '#@data/values\n#@overlay/match missing_ok=True\n---\nimage:\n  repository: %s\n  tag: "%s"\n' \
	    "$$img_repo" "$$img_tag" > $(SUPSVC_BUNDLE)/config/zz-build-values.yaml; \
	ytt -f $(SUPSVC_BUNDLE)/config \
	    | kbld -f - --imgpkg-lock-output $(SUPSVC_BUNDLE)/.imgpkg/images.yml > /dev/null && \
	imgpkg push -b $(BUNDLE_IMG) -f $(SUPSVC_BUNDLE) --lock-output dist/supervisor-bundle.lock.yml; \
	rc=$$?; rm -f $(SUPSVC_BUNDLE)/config/zz-build-values.yaml; exit $$rc
	@echo "Bundle pushed: $(BUNDLE_IMG)"
	@echo "Lock file:     dist/supervisor-bundle.lock.yml"

.PHONY: supervisor-service-yaml
supervisor-service-yaml: ## Emit the upload-ready Supervisor Service YAML (PackageMetadata + Package) to dist/.
	mkdir -p dist
	@if [ -f dist/supervisor-bundle.lock.yml ]; then \
		digest="$$(awk '/image:/{print $$2; exit}' dist/supervisor-bundle.lock.yml)"; \
		echo "Pinning imgpkgBundle to $$digest"; \
	else \
		digest="$(BUNDLE_IMG)"; \
		echo "WARN: no dist/supervisor-bundle.lock.yml found; pinning to tag $$digest (not a digest)."; \
	fi; \
	{ \
		cat $(SUPSVC_PKG_DIR)/package-metadata.yaml; \
		echo; \
		sed -E "s|image: ghcr.io/stanimir-kozarev/vcf-salt-operator-bundle:.*$$|image: $$digest|" \
			$(SUPSVC_PKG_DIR)/package.yaml; \
	} > $(SUPSVC_YAML_OUT)
	@echo "Wrote $(SUPSVC_YAML_OUT)"
	@echo "Service ID: $(SERVICE_ID)  Version: $(VERSION)"

.PHONY: supervisor-release
supervisor-release: docker-build docker-push supervisor-bundle supervisor-service-yaml ## One-shot: build+push operator image, build+push bundle, emit service YAML.
	@echo
	@echo "Upload $(SUPSVC_YAML_OUT) via vSphere Client -> Workload Management -> Services -> Add New Service."

.PHONY: supervisor-relocate
supervisor-relocate: ## Relocate bundle + referenced images to DEST_REPO (e.g. DEST_REPO=nexus.corp/vcf/vcf-salt-operator-bundle). Source = BUNDLE_IMG.
	@command -v imgpkg >/dev/null 2>&1 || { echo "imgpkg not found. Install Carvel imgpkg."; exit 1; }
	@if [ -z "$(DEST_REPO)" ]; then \
		echo "ERROR: DEST_REPO is required."; \
		echo "  Example: make supervisor-relocate BUNDLE_IMG=ghcr.io/stanimir-kozarev/vcf-salt-operator-bundle:0.1.0 \\"; \
		echo "           DEST_REPO=nexus.corp/vcf/vcf-salt-operator-bundle"; \
		exit 2; \
	fi
	mkdir -p dist
	imgpkg copy -b $(BUNDLE_IMG) --to-repo $(DEST_REPO) --lock-output dist/supervisor-bundle.lock.yml
	@echo
	@echo "Bundle + referenced images copied to $(DEST_REPO)."
	@echo "Next: make supervisor-service-yaml   # pins the Package at the Nexus digest"

.PHONY: supervisor-offline-tar
supervisor-offline-tar: ## Pack bundle + images into a single tar (dist/vcf-salt-operator-airgap-<VERSION>.tar) for transfer to an air-gapped lab. Source = BUNDLE_IMG.
	@command -v imgpkg >/dev/null 2>&1 || { echo "imgpkg not found. Install Carvel imgpkg."; exit 1; }
	mkdir -p dist
	imgpkg copy -b $(BUNDLE_IMG) --to-tar dist/vcf-salt-operator-airgap-$(VERSION).tar
	@echo
	@ls -lh dist/vcf-salt-operator-airgap-$(VERSION).tar

.PHONY: supervisor-offline-import
supervisor-offline-import: ## On the lab side: upload a tar to DEST_REPO and pin the Package YAML. Inputs: TAR=<path>, DEST_REPO=<nexus repo>, SERVICE_YAML=<path>.
	@command -v imgpkg >/dev/null 2>&1 || { echo "imgpkg not found. Install Carvel imgpkg."; exit 1; }
	@if [ -z "$(TAR)" ] || [ -z "$(DEST_REPO)" ] || [ -z "$(SERVICE_YAML)" ]; then \
		echo "ERROR: TAR, DEST_REPO, SERVICE_YAML are required."; \
		echo "  Example: make supervisor-offline-import \\"; \
		echo "           TAR=vcf-salt-operator-airgap-0.1.0.tar \\"; \
		echo "           DEST_REPO=nexus.corp/vcf/vcf-salt-operator-bundle \\"; \
		echo "           SERVICE_YAML=vcf-salt-operator-supervisorservice-0.1.0.yaml"; \
		exit 2; \
	fi
	imgpkg copy --tar $(TAR) --to-repo $(DEST_REPO) --lock-output supervisor-bundle.lock.yml
	@digest="$$(awk '/image:/{print $$2; exit}' supervisor-bundle.lock.yml)"; \
	echo "Pinning $(SERVICE_YAML) -> $$digest"; \
	sed -E -i.bak "/imgpkgBundle:/,/image:/ s|image:[[:space:]]+.*vcf-salt-operator-bundle.*|image: $$digest|" $(SERVICE_YAML); \
	rm -f $(SERVICE_YAML).bak
	@echo
	@echo "Done. Upload $(SERVICE_YAML) via vSphere Client -> Workload Management -> Services."

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

GOLANGCI_LINT_VERSION ?= v2.8.0
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
