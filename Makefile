# Copyright 2026 The Kubernetes Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Makefile for application

# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "crd:trivialVersions=true,crdVersions=v1"
# Turn on the CRD_OPTIONS below to generate the v1beta1 version of the Application CRD for kubernetes < 1.16
#CRD_OPTIONS ?= "crd:trivialVersions=true,crdVersions=v1beta1"

# Version/tag — git is the single source of truth (git tags). No VERSION files to
# hand-maintain; `git describe` always reports the real version:
#   - on a release tag commit:      v1.4.2               (exact tag)
#   - N commits past the last tag:  v1.4.2-3-g71627cd15  (tag + distance + sha)
#   - dirty working tree:           ...-dirty
# Release flow: `git tag vX.Y.Z && git push origin vX.Y.Z` — CI builds the tag.
# Override the tag for a one-off build by passing VER=... on the command line.
VER ?= $(shell git describe --tags --dirty --always 2>/dev/null)
ARCH ?= amd64
ALL_ARCH = amd64 arm arm64 ppc64le s390x
IMAGE_NAME = app-controller

# Registry the local `make docker-build`/`make docker-push` targets tag against.
# Defaults to GHCR (same as KO_DOCKER_REPO / CI). The Helm chart pulls from the ops
# Artifact Registry; images land there via `crane copy` (see MEMORY / release docs).
REGISTRY ?= ghcr.io/niq-enterprise

ifeq ($(ARCH), amd64)
IMAGE_TAG ?= $(VER)
else
IMAGE_TAG ?= $(ARCH)-$(VER)
endif

RELEASE_REMOTE ?= origin
RELEASE_TAG ?= $(VER)

CONTROLLER_IMG ?= $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

# Directories.
TOOLS_DIR := $(shell pwd)/hack/tools
TOOLBIN := $(TOOLS_DIR)/bin

# Allow overriding manifest generation destination directory
MANIFEST_ROOT ?= config
CRD_ROOT ?= $(MANIFEST_ROOT)/crd/bases
COVER_FILE ?= cover.out

VERS := dev v0.8.3
.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} \
	  /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: all
all: generate fix vet fmt manifests test lint misspell tidy bin/app-controller ## Run full build pipeline


## --------------------------------------
## Tooling Binaries
## --------------------------------------

$(TOOLBIN)/controller-gen: $(TOOLBIN)/kubectl
	GOBIN=$(TOOLBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.4.0

$(TOOLBIN)/golangci-lint:
	GOBIN=$(TOOLBIN) go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.23.6

$(TOOLBIN)/mockgen:
	GOBIN=$(TOOLBIN) go install github.com/golang/mock/mockgen@v1.3.1

$(TOOLBIN)/conversion-gen:
	GOBIN=$(TOOLBIN) go install k8s.io/code-generator/cmd/conversion-gen@v0.18.9

$(TOOLBIN)/kubebuilder $(TOOLBIN)/etcd $(TOOLBIN)/kube-apiserver $(TOOLBIN)/kubectl:
	cd $(TOOLS_DIR); ./install_kubebuilder.sh
	cp $(TOOLBIN)/kubectl $(HOME)/bin

$(TOOLBIN)/kind:
	GOBIN=$(TOOLBIN) go install sigs.k8s.io/kind@v0.9.0


$(TOOLBIN)/misspell:
	GOBIN=$(TOOLBIN) go install github.com/client9/misspell/cmd/misspell@v0.3.4

.PHONY: install-tools
install-tools: ## Install all tool binaries into hack/tools/bin
install-tools: \
	$(TOOLBIN)/controller-gen \
	$(TOOLBIN)/golangci-lint \
	$(TOOLBIN)/mockgen \
	$(TOOLBIN)/conversion-gen \
	$(TOOLBIN)/kubebuilder \
	$(TOOLBIN)/misspell \
	$(TOOLBIN)/kind

## --------------------------------------
## Tests
## --------------------------------------

# Run tests
.PHONY: test
test: $(TOOLBIN)/etcd $(TOOLBIN)/kube-apiserver $(TOOLBIN)/kubectl ## Run unit tests with envtest
	TEST_ASSET_KUBECTL=$(TOOLBIN)/kubectl \
	TEST_ASSET_KUBE_APISERVER=$(TOOLBIN)/kube-apiserver \
	TEST_ASSET_ETCD=$(TOOLBIN)/etcd \
	go test -v ./api/... ./controllers/... -coverprofile $(COVER_FILE)

# Run e2e-tests (envtest-based, no cluster required)
.PHONY: e2e-test
e2e-test: generate fmt vet $(TOOLBIN)/etcd $(TOOLBIN)/kube-apiserver $(TOOLBIN)/kubectl ## Run e2e tests (envtest, no cluster needed)
	TEST_ASSET_KUBECTL=$(TOOLBIN)/kubectl \
	TEST_ASSET_KUBE_APISERVER=$(TOOLBIN)/kube-apiserver \
	TEST_ASSET_ETCD=$(TOOLBIN)/etcd \
	go test -v ./e2e/...

## --------------------------------------
## Build and run
## --------------------------------------

# Build app-controller binary
bin/app-controller: main.go ## Build the controller binary
	go build -o bin/app-controller main.go

# Run against the configured Kubernetes cluster in ~/.kube/config
.PHONY: runbg
runbg: bin/app-controller ## Run controller in background, logs to app-controller.log
	bin/app-controller --metrics-addr ":8083" >& app-controller.log & echo $$! > app-controller.pid

# Run against the configured Kubernetes cluster in ~/.kube/config
.PHONY: run
run: bin/app-controller ## Run controller against current kubeconfig
	bin/app-controller

# Debug using the configured Kubernetes cluster in ~/.kube/config
.PHONY: debug
debug: generate fmt vet manifests ## Debug controller with dlv
	dlv debug ./main.go


## --------------------------------------
## Code maintenance
## --------------------------------------

.PHONY: fmt
fmt: ## Run go fmt
	go fmt ./api/... ./controllers/...

.PHONY: vet
vet: ## Run go vet
	go vet ./api/... ./controllers/...

.PHONY: fix
fix: ## Run go fix
	go fix ./api/... ./controllers/...


.PHONY: tidy
tidy: ## Run go mod tidy
	go mod tidy

.PHONY: lint
lint: $(TOOLBIN)/golangci-lint ## Run golangci-lint
	$(TOOLBIN)/golangci-lint run ./...

.PHONY: misspell
misspell: $(TOOLBIN)/misspell
	$(TOOLBIN)/misspell ./**

.PHONY: misspell-fix
misspell-fix: $(TOOLBIN)/misspell
	$(TOOLBIN)/misspell -w ./**


## --------------------------------------
## Deploy all (CRDs + Controller)
## --------------------------------------

HELM_CHART ?= charts/app-controller
HELM_RELEASE ?= app
HELM_NAMESPACE ?= application-system

# Deploy controller via the Helm chart to the cluster in ~/.kube/config.
.PHONY: deploy
deploy: ## Deploy CRD + controller to current cluster (Helm)
	helm upgrade --install $(HELM_RELEASE) $(HELM_CHART) -n $(HELM_NAMESPACE) --create-namespace

.PHONY: undeploy
undeploy: ## Remove the controller release from current cluster
	helm uninstall $(HELM_RELEASE) -n $(HELM_NAMESPACE)

.PHONY: deploy-crd
deploy-crd: $(TOOLBIN)/kubectl ## Install the Application CRD into current cluster
	$(TOOLBIN)/kubectl apply -f config/crd/bases/app.k8s.io_applications.yaml

.PHONY: undeploy-crd
undeploy-crd: $(TOOLBIN)/kubectl ## Uninstall the Application CRD from current cluster
	$(TOOLBIN)/kubectl delete -f config/crd/bases/app.k8s.io_applications.yaml

## --------------------------------------
## Generating
## --------------------------------------

.PHONY: generate
generate: ## Generate code + manifests, and sync the CRD into the Helm chart
	$(MAKE) generate-go
	$(MAKE) manifests
	cp config/crd/bases/app.k8s.io_applications.yaml charts/app-controller/crds/

# Generate manifests e.g. CRD, RBAC etc.
.PHONY: manifests
manifests: $(TOOLBIN)/controller-gen ## Regenerate the Application CRD
	$(TOOLBIN)/controller-gen \
		$(CRD_OPTIONS) \
		paths=./api/... \
		output:crd:artifacts:config=$(CRD_ROOT) \
		output:crd:dir=$(CRD_ROOT)
	@for f in config/crd/bases/*.yaml; do \
		kubectl annotate --overwrite -f $$f --local=true -o yaml api-approved.kubernetes.io=https://github.com/kubernetes-sigs/application/pull/2 > $$f.bk; \
		mv $$f.bk $$f; \
	done

.PHONY: generate-go
generate-go: $(TOOLBIN)/controller-gen $(TOOLBIN)/conversion-gen  $(TOOLBIN)/mockgen
	go generate ./api/... ./controllers/...
	$(TOOLBIN)/controller-gen \
		paths=./api/v1beta1/... \
		object:headerFile=./hack/boilerplate.go.txt

## --------------------------------------
## Docker
## --------------------------------------
.PHONY: docker-build
docker-build: test ## Build the docker image for app-controller
	docker build --network=host --pull --build-arg ARCH=$(ARCH) . -t $(CONTROLLER_IMG)

.PHONY: docker-push
docker-push: ## Push the docker image
	docker push $(CONTROLLER_IMG)

## --------------------------------------
## ko
## --------------------------------------

KO_DOCKER_REPO ?= ghcr.io/niq-enterprise/symbio-application-controller

.PHONY: ko-image
ko-image: ## Build container image locally using ko
	KO_DOCKER_REPO=ko.local ko build \
		--oci-layout-path=bin/images/app-controller \
		.

.PHONY: ko-push
ko-push: ## Build and push container image using ko
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) ko build \
		--tags=$(VER),latest \
		--bare \
		--image-label org.opencontainers.image.version=$(VER) \
		.

.PHONY: clean
clean: ## Remove build artifacts and tool binaries
	go clean --cache
	rm -f $(COVER_FILE)
	rm -f $(TOOLBIN)/goimports
	rm -f $(TOOLBIN)/golangci-lint
	rm -f $(TOOLBIN)/controller-gen
	rm -f $(TOOLBIN)/conversion-gen
	rm -f $(TOOLBIN)/etcd
	rm -f $(TOOLBIN)/kube-apiserver
	rm -f $(TOOLBIN)/kubebuilder
	rm -f $(TOOLBIN)/kubectl
	rm -f $(TOOLBIN)/misspell
	rm -f $(TOOLBIN)/mockgen
	rm -f $(TOOLBIN)/kind


## --------------------------------------
## Releasing
## --------------------------------------
# Git tags are the source of truth. To cut a release, tag a commit on main and push
# the tag — CI (.github/workflows/ci.yaml) builds the multi-arch binaries + image and
# publishes the GitHub Release. Pass the version explicitly:
#   make release-tag RELEASE_TAG=v1.5.0

.PHONY: release-tag
release-tag: ## Tag the current commit and push it (RELEASE_TAG=vX.Y.Z), triggering the CI release
	@case "$(RELEASE_TAG)" in \
		v[0-9]*.[0-9]*.[0-9]*) ;; \
		*) echo "RELEASE_TAG must be an explicit semver tag, e.g. make release-tag RELEASE_TAG=v1.5.0 (got '$(RELEASE_TAG)')"; exit 1 ;; \
	esac
	git tag -a $(RELEASE_TAG) -m "Release $(RELEASE_TAG)"
	git push $(RELEASE_REMOTE) $(RELEASE_TAG)

.PHONY: delete-release-tag
delete-release-tag: ## Delete a release tag locally and on the remote (RELEASE_TAG=vX.Y.Z)
	git tag --delete $(RELEASE_TAG)
	git push $(RELEASE_REMOTE) :refs/tags/$(RELEASE_TAG)
