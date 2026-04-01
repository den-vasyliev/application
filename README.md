[![CI](https://github.com/den-vasyliev/application/actions/workflows/ci.yaml/badge.svg)](https://github.com/den-vasyliev/application/actions/workflows/ci.yaml)
[![Go Report Card](https://goreportcard.com/badge/sigs.k8s.io/application)](https://goreportcard.com/report/sigs.k8s.io/application)

# Kubernetes Application Controller

A Kubernetes CRD controller that provides an application-centric abstraction over raw Kubernetes resources. An `Application` resource groups related components (Deployments, StatefulSets, Services, Argo Rollouts, etc.) and aggregates their health into a single status.

## What's new in v1.0.0

- **Go 1.24** — modernized from Go 1.13; all dependencies updated (k8s 0.31, controller-runtime 0.19)
- **Argo Rollout support** — `Rollout.argoproj.io` status is now tracked via `status.phase` (`Healthy`→Ready, `Degraded`/`Paused`→InProgress). Previously degraded Rollouts were incorrectly reported as Ready.
- **Envtest-based tests** — e2e suite replaced with envtest (no Kind cluster or real kubeconfig required)
- **GitHub Actions CI** — replaces Travis CI; publishes multi-arch binaries on every tagged release

## Supported component types

| Group | Kind | Ready when |
|-------|------|------------|
| `apps` | Deployment | all replicas available, no ReplicaFailure |
| `apps` | StatefulSet | all replicas current and ready |
| `apps` | ReplicaSet | all replicas available |
| `apps` | DaemonSet | all scheduled pods ready |
| `argoproj.io` | **Rollout** | `status.phase == Healthy` |
| `batch` | Job | started (`startTime` set) |
| `policy` | PodDisruptionBudget | currentHealthy ≥ desiredHealthy |
| _(core)_ | Pod | PodReady condition True |
| _(core)_ | Service | ClusterIP/NodePort/ExternalName always; LoadBalancer once IP assigned |
| _(core)_ | PersistentVolumeClaim | phase Bound |
| _(core)_ | ReplicationController | all replicas ready |
| _custom_ | anything | standard `Ready`/`InProgress` conditions |

## Install

```bash
kubectl apply -f https://github.com/den-vasyliev/application/releases/latest/download/kube-app-manager-aio.yaml
```

## Running locally

```bash
# Download binary
curl -L https://github.com/den-vasyliev/application/releases/latest/download/kube-app-manager-$(uname -s | tr '[:upper:]' '[:lower:]')-amd64 -o kube-app-manager
chmod +x kube-app-manager

# Run against current kubeconfig
./kube-app-manager --metrics-addr :8080 --sync-period 120
```

## Example Application

```yaml
apiVersion: app.k8s.io/v1beta1
kind: Application
metadata:
  name: my-app
  namespace: default
spec:
  selector:
    matchLabels:
      app: my-app
  componentKinds:
    - group: apps
      kind: Deployment
    - group: argoproj.io
      kind: Rollout
    - group: ""
      kind: Service
```

## Building from source

```bash
git clone https://github.com/den-vasyliev/application
cd application
go build -o kube-app-manager main.go
```

## Releasing

Push a `v*` tag to trigger the CI release pipeline:

```bash
git tag v1.x.y && git push origin v1.x.y
```

GitHub Actions builds `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64` binaries with SHA256 checksums and attaches them to the GitHub Release.
