[![CI](https://github.com/den-vasyliev/application/actions/workflows/ci.yaml/badge.svg)](https://github.com/den-vasyliev/application/actions/workflows/ci.yaml)

# Application Controller

A Kubernetes controller for the `Application` custom resource (`app.k8s.io/v1beta1`).
It:

- **Groups related workloads** into one `Application` — Deployments, StatefulSets,
  Services, Argo Rollouts, Jobs, and more — selected by labels and component kinds.
- **Aggregates their health** into a single `Ready` status, with anti-flap
  semantics: a workload counts as healthy while it is serving, not only at full
  desired count, so scale-ups and rolling updates don't churn the status.
- Optionally **[pushes to a triage agent](#push-mode)** over an outbound WebSocket,
  so clusters with no reachable API can still report their state.

## Install

```bash
helm install app charts/app-controller -n application-system --create-namespace
```

The chart bundles the CRD and has no `kube-rbac-proxy` sidecar; metrics are off by
default (`--metrics-addr=0`).

## Use

```yaml
apiVersion: app.k8s.io/v1beta1
kind: Application
metadata:
  name: my-app
spec:
  selector:
    matchLabels:
      app: my-app
  componentKinds:
    - { group: apps, kind: Deployment }
    - { group: "", kind: Service }
```

The controller watches the components matched by `selector` + `componentKinds` and
sets the Application's `Ready` condition from their combined health.

### Component readiness

Scalable workloads are Ready when they are **serving**, not when at full desired
count — an in-progress HPA scale-up or rolling update does not flap the Application
to `InProgress`. Scaled to zero (`spec.replicas: 0`) is Ready.

| Group | Kind | Ready when |
|-------|------|------------|
| `apps` | Deployment | `Available` condition true, no `ReplicaFailure` |
| `apps` | StatefulSet | `ReadyReplicas == CurrentReplicas` (managed pods, not the desired count) |
| `apps` | ReplicaSet | `AvailableReplicas > 0`, no `ReplicaFailure` |
| `apps` | DaemonSet | `NumberUnavailable == 0` (respects `maxUnavailable`) |
| `argoproj.io` | Rollout | phase `Healthy` / `Inactive` / `Progressing` / `Paused` (serving); `Degraded` / `Error` → InProgress |
| `batch` | Job | started (`startTime` set) |
| `batch` | CronJob | always, unless `spec.suspend: true` |
| `policy` | PodDisruptionBudget | `currentHealthy >= desiredHealthy` |
| _core_ | Pod | `PodReady` condition true |
| _core_ | Service | ClusterIP/NodePort/ExternalName always; LoadBalancer once an IP is assigned |
| _core_ | PersistentVolumeClaim | phase `Bound` |
| _core_ | ReplicationController | `AvailableReplicas > 0` |
| custom | anything | standard `Ready` / `InProgress` conditions |

## Push mode

For a cluster a triage agent cannot reach — behind a firewall or NAT — the
controller dials **out** to the agent and streams its Applications, status changes,
and Kubernetes Warning events over a WebSocket.

```bash
helm upgrade --install app charts/app-controller -n application-system \
  --set push.enabled=true \
  --set push.endpoint=wss://<triage-host>/events/ws \
  --set push.clusterName=<cluster> \
  --set push.token=<bearer-token>
```

The endpoint must be `wss://` — a plaintext token is refused at startup. See the
chart [values](charts/app-controller/values.yaml) for all push settings, and
[ADR-0005](doc/adr/0005-outbound-push-mode.md) for the design.

## Development

```bash
make bin/app-controller        # build
make test                      # unit tests (envtest, no cluster needed)
make manifests                 # regenerate the CRD
make generate                  # manifests + sync CRD into the chart

# Run against the current kubeconfig:
./bin/app-controller --metrics-addr=0
```

Release notes: [CHANGELOG.md](CHANGELOG.md). Push a `v*` tag to cut a release.
