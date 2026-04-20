# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the **Kubernetes Application** controller — a Kubernetes CRD controller (operator pattern) that provides an application-centric abstraction over raw Kubernetes resources. An `Application` resource groups and monitors related Kubernetes components (Deployments, StatefulSets, Services, Argo Rollouts, etc.) and aggregates their health into a single application status.

Repository: https://github.com/den-vasyliev/application (default branch: `main`)

## Commands

```bash
# Build
make bin/kube-app-manager        # build binary (just go build, no codegen)
make all                         # fmt, vet, lint, test, build

# Run locally against current kubeconfig
./bin/kube-app-manager

# Test (requires etcd/kube-apiserver/kubectl test assets in $(TOOLBIN))
make test                        # go test ./api/... ./controllers/... -coverprofile cover.out

# Single test (Ginkgo focus filter)
go test -v ./controllers/... -run "TestAPIs" --ginkgo.focus="<test description>"

# E2E tests (envtest-based, no cluster required — same test asset deps as make test)
make e2e-test

# Code quality
make lint                        # golangci-lint
make fmt                         # go fmt
make vet                         # go vet

# Code generation
make manifests                   # regenerate CRD/RBAC/webhook manifests via controller-gen
make generate                    # run generate-go + manifests + generate-resources
```

Test assets (etcd, kube-apiserver, kubectl) must be present at the paths set by `TEST_ASSET_ETCD`, `TEST_ASSET_KUBE_APISERVER`, `TEST_ASSET_KUBECTL` env vars (defaulting to `$(TOOLBIN)/`).

## Architecture

### API (`api/v1beta1/`)

- `application_types.go` — defines the `Application` CRD: `ApplicationSpec` (component selector, descriptor, assembly phase, info) and `ApplicationStatus` (observed components, readiness conditions)
- Status conditions: `Ready`, `Qualified`, `Settled`, `Cleanup`, `Error`
- `ComponentList` tracks the status of every matched Kubernetes resource

### Controller (`controllers/`)

- `application_controller.go` — `ApplicationReconciler.Reconcile(ctx, req)` is the main loop:
  1. Fetch the `Application` resource
  2. Call `updateComponents()` to list all Kubernetes resources matching the Application's label selectors
  3. Call `getNewApplicationStatus()` to aggregate component health into application-level conditions
  4. Patch the `Application` status

- `status.go` — `status()` dispatches per-resource readiness computation. Handled types:
  - Standard k8s: Deployment, StatefulSet, ReplicaSet, DaemonSet, Pod, Service, PVC, PodDisruptionBudget, ReplicationController, Job
  - **Argo Rollout** (`Rollout.argoproj.io`): reads `status.phase` — `Healthy`/`Inactive`→Ready, `Degraded`/`Progressing`/`Paused`/`Error`→InProgress
  - Everything else: `statusFromStandardConditions` (checks `Ready`/`InProgress` condition types)

- `condition.go` — helpers for setting/clearing `ApplicationCondition` entries on the status

### Main Entry (`main.go`)

Sets up the `controller-runtime` manager, registers the `ApplicationReconciler`, and starts the leader-elected control loop. Flags: `--metrics-addr`, `--enable-leader-election`, `--namespace`, `--sync-period`.

### Testing

Both `controllers/` and `e2e/` use **Ginkgo v2** BDD style with `envtest` (embedded etcd + kube-apiserver, no real cluster needed). Suite setup is in `controllers/suite_test.go` and `e2e/main_test.go`. The e2e suite focuses specifically on custom CRD component tracking (registers a `TestCRD` from `e2e/testdata/test_crd.yaml`).

### CI (`./github/workflows/ci.yaml`)

- **Every push/PR to `main`**: `go vet` + `go build`
- **Tag `v*`**: build multi-arch binaries (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`) + create GitHub Release with binaries and SHA256 checksums

To release: `git tag v0.x.y && git push den v0.x.y`
