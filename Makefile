# Copyright 2026 The Kubernetes Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Makefile for application

VERSION_FILE ?= VERSION-DEV

include $(VERSION_FILE)

# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "crd:trivialVersions=true,crdVersions=v1"
# Turn on the CRD_OPTIONS below to generate the v1beta1 version of the Application CRD for kubernetes < 1.16
#CRD_OPTIONS ?= "crd:trivialVersions=true,crdVersions=v1beta1"

# Releases should modify and double check these vars.
VER ?= v${app_major}.${app_minor}.${app_patch}
ARCH ?= amd64
ALL_ARCH = amd64 arm arm64 ppc64le s390x
IMAGE_NAME = kube-app-manager

ifeq ($(ARCH), amd64)
IMAGE_TAG ?= $(VER)
else
IMAGE_TAG ?= $(ARCH)-$(VER)
endif

RELEASE_REMOTE ?= origin
RELEASE_BRANCH ?= release-v${app_major}.${app_minor}
RELEASE_TAG ?= v${app_major}.${app_minor}.${app_patch}

CONTROLLER_IMG ?= $(REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

# Directories.
TOOLS_DIR := $(shell pwd)/hack/tools
TOOLBIN := $(TOOLS_DIR)/bin

# Allow overriding manifest generation destination directory
MANIFEST_ROOT ?= config
CRD_ROOT ?= $(MANIFEST_ROOT)/crd/bases
WEBHOOK_ROOT ?= $(MANIFEST_ROOT)/webhook
RBAC_ROOT ?= $(MANIFEST_ROOT)/rbac
COVER_FILE ?= cover.out

VERS := dev v0.8.3
.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} \
	  /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: all
all: generate fix vet fmt manifests test lint misspell tidy bin/kube-app-manager ## Run full build pipeline


## --------------------------------------
## Tooling Binaries
## --------------------------------------

$(TOOLBIN)/controller-gen: $(TOOLBIN)/kubectl
	GOBIN=$(TOOLBIN) GO111MODULE=on go get sigs.k8s.io/controller-tools/cmd/controller-gen@v0.4.0

$(TOOLBIN)/golangci-lint:
	GOBIN=$(TOOLBIN) GO111MODULE=on go get github.com/golangci/golangci-lint/cmd/golangci-lint@v1.23.6

$(TOOLBIN)/mockgen:
	GOBIN=$(TOOLBIN) GO111MODULE=on go get github.com/golang/mock/mockgen@v1.3.1

$(TOOLBIN)/conversion-gen:
	GOBIN=$(TOOLBIN) GO111MODULE=on go get k8s.io/code-generator/cmd/conversion-gen@v0.18.9

$(TOOLBIN)/kubebuilder $(TOOLBIN)/etcd $(TOOLBIN)/kube-apiserver $(TOOLBIN)/kubectl:
	cd $(TOOLS_DIR); ./install_kubebuilder.sh
	cp $(TOOLBIN)/kubectl $(HOME)/bin

$(TOOLBIN)/kustomize:
	cd $(TOOLS_DIR); ./install_kustomize.sh

$(TOOLBIN)/kind:
	GOBIN=$(TOOLBIN) GO111MODULE=on go get sigs.k8s.io/kind@v0.9.0


$(TOOLBIN)/misspell:
	GOBIN=$(TOOLBIN) GO111MODULE=on go get github.com/client9/misspell/cmd/misspell@v0.3.4

.PHONY: install-tools
install-tools: ## Install all tool binaries into hack/tools/bin
install-tools: \
	$(TOOLBIN)/controller-gen \
	$(TOOLBIN)/golangci-lint \
	$(TOOLBIN)/mockgen \
	$(TOOLBIN)/conversion-gen \
	$(TOOLBIN)/kubebuilder \
	$(TOOLBIN)/kustomize \
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

# Build kube-app-kube-app-manager binary
bin/kube-app-manager: main.go ## Build the controller binary
	go build -o bin/kube-app-manager main.go

# Run against the configured Kubernetes cluster in ~/.kube/config
.PHONY: runbg
runbg: bin/kube-app-manager ## Run controller in background, logs to kube-app-manager.log
	bin/kube-app-manager --metrics-addr ":8083" >& kube-app-manager.log & echo $$! > kube-app-manager.pid

# Run against the configured Kubernetes cluster in ~/.kube/config
.PHONY: run
run: bin/kube-app-manager ## Run controller against current kubeconfig
	bin/kube-app-manager

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

# Deploy controller in the configured Kubernetes cluster in ~/.kube/config
# This is expected to be used by user during dev
.PHONY: deploy
deploy: ## Deploy CRDs + controller to current cluster
	kubectl apply -f deploy/kube-app-manager-aio.yaml

.PHONY: undeploy
undeploy: ## Remove CRDs + controller from current cluster
	kubectl delete -f deploy/kube-app-manager-aio.yaml

.PHONY: deploy-dev
deploy-dev: $(TOOLBIN)/kubectl generate-resources
	$(TOOLBIN)/kubectl apply -f $(AIO_YAML)

# unDeploy controller in the configured Kubernetes cluster in ~/.kube/config
.PHONY: undeploy-dev
undeploy-dev: $(TOOLBIN)/kubectl generate-resources
		$(TOOLBIN)/kubectl delete -f $(AIO_YAML)

## --------------------------------------
## Deploy CRDs only
## --------------------------------------
# Install CRDs into a cluster,
.PHONY: deploy-crd
deploy-crd: $(TOOLBIN)/kustomize $(TOOLBIN)/kubectl ## Install CRDs into current cluster
	$(TOOLBIN)/kustomize build config/crd| $(TOOLBIN)/kubectl apply -f -

.PHONY: undeploy-crd
undeploy-crd: $(TOOLBIN)/kustomize $(TOOLBIN)/kubectl ## Uninstall CRDs from current cluster
	$(TOOLBIN)/kustomize build config/crd| $(TOOLBIN)/kubectl delete -f -

## --------------------------------------
## Deploy demo
## --------------------------------------

# Deploy wordpress
.PHONY: deploy-wordpress
deploy-wordpress: $(TOOLBIN)/kustomize $(TOOLBIN)/kubectl
	mkdir -p /tmp/data1 /tmp/data2
	$(TOOLBIN)/kustomize build docs/examples/wordpress | $(TOOLBIN)/kubectl apply -f -

# Uneploy wordpress
.PHONY: undeploy-wordpress
undeploy-wordpress: $(TOOLBIN)/kustomize $(TOOLBIN)/kubectl
	$(TOOLBIN)/kustomize build docs/examples/wordpress | $(TOOLBIN)/kubectl delete -f -
	# $(TOOLBIN)/kubectl delete pvc --all
	# sudo rm -fr /tmp/data1 /tmp/data2

## --------------------------------------
## Generating
## --------------------------------------

.PHONY: generate
generate: ## Generate code
	$(MAKE) generate-go
	$(MAKE) manifests
	$(MAKE) generate-resources
	VERSION_FILE=VERSION $(MAKE) generate-resources

# Generate manifests e.g. CRD, RBAC etc.
.PHONY: manifests
manifests: $(TOOLBIN)/controller-gen ## Regenerate CRD/RBAC/webhook manifests
	$(TOOLBIN)/controller-gen \
		$(CRD_OPTIONS) \
		rbac:roleName=kube-app-manager-role \
		paths=./... \
		output:crd:artifacts:config=$(CRD_ROOT) \
		output:crd:dir=$(CRD_ROOT) \
		output:webhook:dir=$(WEBHOOK_ROOT) \
		webhook
	@for f in config/crd/bases/*.yaml; do \
		kubectl annotate --overwrite -f $$f --local=true -o yaml api-approved.kubernetes.io=https://github.com/kubernetes-sigs/application/pull/2 > $$f.bk; \
		mv $$f.bk $$f; \
	done

.PHONY: generate-resources
generate-resources: $(TOOLBIN)/kustomize
	cd config/default/scratch && $(TOOLBIN)/kustomize edit set image kube-app-manager=$(CONTROLLER_IMG)
	$(TOOLBIN)/kustomize build config/default/scratch/ -o $(AIO_YAML)

.PHONY: generate-go
generate-go: $(TOOLBIN)/controller-gen $(TOOLBIN)/conversion-gen  $(TOOLBIN)/mockgen
	go generate ./api/... ./controllers/...
	$(TOOLBIN)/controller-gen \
		paths=./api/v1beta1/... \
		object:headerFile=./hack/boilerplate.go.txt

## --------------------------------------
## Docker
## --------------------------------------
.PHONY: set-image
set-image: $(TOOLBIN)/kustomize
	@echo "updating kustomize image patch file for kube-app-manager resource"
	cd config/kube-app-manager && $(TOOLBIN)/kustomize edit set image kube-app-manager=$(CONTROLLER_IMG)

.PHONY: docker-build
docker-build: set-image test $(TOOLBIN)/kustomize ## Build the docker image for kube-app-manager
	docker build --network=host --pull --build-arg ARCH=$(ARCH) . -t $(CONTROLLER_IMG)

.PHONY: docker-push
docker-push: ## Push the docker image
	docker push $(CONTROLLER_IMG)

## --------------------------------------
## ko
## --------------------------------------

KO_DOCKER_REPO ?= ghcr.io/den-vasyliev/application

.PHONY: ko-image
ko-image: ## Build container image locally using ko
	KO_DOCKER_REPO=ko.local ko build \
		--oci-layout-path=bin/images/kube-app-manager \
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
	rm -f $(TOOLBIN)/kustomize
	rm -f $(TOOLBIN)/goimports
	rm -f $(TOOLBIN)/golangci-lint
	rm -f $(TOOLBIN)/controller-gen
	rm -f $(TOOLBIN)/conversion-gen
	rm -f $(TOOLBIN)/etcd
	rm -f $(TOOLBIN)/kube-apiserver
	rm -f $(TOOLBIN)/kubebuilder
	rm -f $(TOOLBIN)/kubectl
	rm -f $(TOOLBIN)/kustomize
	rm -f $(TOOLBIN)/misspell
	rm -f $(TOOLBIN)/mockgen
	rm -f $(TOOLBIN)/kind


## --------------------------------------
## Version bumping
## --------------------------------------

.PHONY: bump-patch
bump-patch: ## Bump patch version, commit, tag, and push
	$(MAKE) VERSION_FILE=VERSION _bump-patch

.PHONY: _bump-patch
_bump-patch:
	@NEW=$(shell echo $$(($(app_patch)+1))); \
	awk -v old=$(app_patch) -v new=$$NEW \
		'{gsub("export app_patch=" old, "export app_patch=" new)}1' \
		VERSION > VERSION.tmp && mv VERSION.tmp VERSION; \
	git add VERSION; \
	git commit -m "release: bump patch to v$(app_major).$(app_minor).$$NEW"; \
	git tag v$(app_major).$(app_minor).$$NEW; \
	git push origin main; \
	git push origin v$(app_major).$(app_minor).$$NEW

.PHONY: bump-minor
bump-minor: ## Bump minor version (reset patch to 0), commit, tag, and push
	$(MAKE) VERSION_FILE=VERSION _bump-minor

.PHONY: _bump-minor
_bump-minor:
	@NEW=$(shell echo $$(($(app_minor)+1))); \
	awk -v om=$(app_minor) -v nm=$$NEW \
		-v op=$(app_patch) \
		'{gsub("export app_minor=" om, "export app_minor=" nm); \
		  gsub("export app_patch=" op, "export app_patch=0")}1' \
		VERSION > VERSION.tmp && mv VERSION.tmp VERSION; \
	git add VERSION; \
	git commit -m "release: bump minor to v$(app_major).$$NEW.0"; \
	git tag v$(app_major).$$NEW.0; \
	git push origin main; \
	git push origin v$(app_major).$$NEW.0

## --------------------------------------
## Releasing
## --------------------------------------
.PHONY: release-branch
release-branch:
	echo "checking branch=$(RELEASE_BRANCH)"
	git ls-remote --exit-code `git remote get-url $(RELEASE_REMOTE)` $(RELEASE_BRANCH) || make create-release-branch

.PHONY: create-release-branch
create-release-branch:
	git fetch upstream
	git checkout main
	git rebase upstream/main
	git branch -D $(RELEASE_BRANCH) || true
	git checkout -b $(RELEASE_BRANCH)
	git push -f $(RELEASE_REMOTE) $(RELEASE_BRANCH)

.PHONY: release-tag
release-tag: release-branch
	git branch -D $(RELEASE_BRANCH) || true
	git branch ${RELEASE_BRANCH} ${RELEASE_REMOTE}/${RELEASE_BRANCH}
	git checkout $(RELEASE_BRANCH)
	git tag -a ${RELEASE_TAG} -m "Release ${RELEASE_TAG} on branch ${RELEASE_BRANCH}"
	git push $(RELEASE_REMOTE) ${RELEASE_TAG}

.PHONY: delete-release-tag
delete-release-tag:
	git tag --delete $(RELEASE_TAG)
	git push $(RELEASE_REMOTE) :refs/tags/$(RELEASE_TAG)
