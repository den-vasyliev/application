# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is the **Kubernetes Application** controller — a Kubernetes CRD controller (operator pattern) that provides an application-centric abstraction over raw Kubernetes resources. An `Application` resource groups and monitors related Kubernetes components (Deployments, StatefulSets, Services, Argo Rollouts, etc.) and aggregates their health into a single application status.

Repository: https://github.com/den-vasyliev/application (default branch: `main`)

## Commands

```bash
# Build
make bin/app-controller        # build binary (just go build, no codegen)
make all                         # fmt, vet, lint, test, build

# Run locally against current kubeconfig
./bin/app-controller

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
make manifests                   # regenerate the Application CRD via controller-gen
make generate                    # generate-go + manifests + sync CRD into the chart
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
  2. Call `ensureComponentWatches()` to register a real-time watch per `spec.componentKinds` GVK (deduped, dynamic — see ADR-0004), so component status changes trigger a reconcile within seconds instead of waiting for the `--sync-period` resync
  3. Call `updateComponents()` to list all Kubernetes resources matching the Application's label selectors
  4. Call `getNewApplicationStatus()` to aggregate component health into application-level conditions
  5. Patch the `Application` status
  - `SetupWithManager` uses `builder.Build` (not `.Complete`) to keep the controller + cache handles needed for the dynamic watches; `applicationsForComponent` maps a changed component → matching Applications.

- `status.go` — `status()` dispatches per-resource readiness computation. Handled types:
  - Standard k8s: Deployment, StatefulSet, ReplicaSet, DaemonSet, Pod, Service, PVC, PodDisruptionBudget, ReplicationController, Job, CronJob
  - **Scalable workloads are Ready when *serving*, not when at full desired count** — so an HPA scale-up (e.g. 2→3) does not flap the app to `InProgress` while the new pod passes its readiness probe (see ADR-0003):
    - **Deployment**: `Available` condition true + no `ReplicaFailure` (does not require `ReadyReplicas == spec.replicas`)
    - **StatefulSet**: `ReadyReplicas == CurrentReplicas` (pods the controller is actually managing, not the desired spec count)
    - **ReplicaSet**: `AvailableReplicas > 0` + no `ReplicaFailure`
    - **ReplicationController**: `AvailableReplicas > 0`
    - **DaemonSet**: `NumberUnavailable == 0` (respects `maxUnavailable`, so a node join doesn't flap)
    - All preserve scaled-to-zero (`spec.replicas=0`) → Ready, and still report `InProgress` on genuine failure (nothing available / `ReplicaFailure` / unavailable pods)
  - **CronJob**: Ready unless `spec.suspend=true` — schedule/success history does not affect app health
  - **Argo Rollout** (`Rollout.argoproj.io`): reads `status.phase` — `Healthy`/`Inactive`/`Progressing`/`Paused`→Ready (a scaling/canary/paused Rollout is still serving), `Degraded`/`Error`→InProgress. Scaled-to-zero (`spec.replicas=0`) → Ready regardless of phase, so a parked Rollout with a `Degraded`/`InvalidSpec` phase doesn't degrade its app (see ADR-0003)
  - Everything else: `statusFromStandardConditions` (checks `Ready`/`InProgress` condition types)

- `condition.go` — helpers for setting/clearing `ApplicationCondition` entries on the status

### Main Entry (`main.go`)

Sets up the `controller-runtime` manager, registers the `ApplicationReconciler`, and starts the control loop. Flags: `--metrics-addr` (`0` disables), `--enable-leader-election`, `--namespace`, `--sync-period`, plus the push-mode flags (`--push-endpoint`, `--cluster-name`, `--push-token[-file]`, `--push-namespaces`, `--push-heartbeat`, `--push-insecure-skip-verify`).

### Push mode (`push/`)

Opt-in outbound-WebSocket streaming of Applications + Kubernetes Warning events to a triage agent, for clusters with no inbound API access (ADR-0005). `push.New()` returns nil unless `--push-endpoint` is set, so it is a no-op by default. `push.Pusher` is a `manager.Runnable` that reuses the manager cache/informers — it does **not** touch `controllers/`. The wire frames (`push/protocol.go`) mirror the triage receiver's `internal/remoteagent/protocol.go` (separate modules; keep them in sync).

### Deploy

`charts/app-controller` (Helm) is the install path — bundles the CRD, toggles push mode + metrics, ships its own RBAC, and has no `kube-rbac-proxy` sidecar. The only thing left under `config/` is the CRD source (`config/crd/bases/`), regenerated by `make manifests` and synced into the chart by `make generate`. The old kustomize overlays, rbac-proxy patch, prometheus config, aio manifest, and wordpress demo were removed.

### Testing

Both `controllers/` and `e2e/` use **Ginkgo v2** BDD style with `envtest` (embedded etcd + kube-apiserver, no real cluster needed). Suite setup is in `controllers/suite_test.go` and `e2e/main_test.go`. The e2e suite focuses specifically on custom CRD component tracking (registers a `TestCRD` from `e2e/testdata/test_crd.yaml`).

### CI (`./github/workflows/ci.yaml`)

- **Every push/PR to `main`**: `go vet` + `go build`
- **Tag `v*`**: build multi-arch binaries (`linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`) + create GitHub Release with binaries and SHA256 checksums

To release: `git tag v0.x.y && git push den v0.x.y`
