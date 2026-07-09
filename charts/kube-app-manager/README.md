# kube-app-manager Helm chart

Installs the Kubernetes Application controller (`app.k8s.io/v1beta1`) with optional
**push mode** — streaming Application inventory + Kubernetes Warning events to a
triage agent over an outbound WebSocket for clusters with no inbound API access
(see [ADR-0005](../../doc/adr/0005-outbound-push-mode.md)).

No `kube-rbac-proxy` sidecar and metrics disabled by default — the two legacy
kubebuilder-scaffold extras. Enable metrics explicitly if you scrape them.

## Install

```bash
helm install app charts/kube-app-manager -n application-system --create-namespace
```

The Application CRD ships in `crds/` and is installed automatically on first
install. (Helm does not upgrade CRDs — apply CRD changes manually.)

## Push mode

```bash
helm upgrade --install app charts/kube-app-manager -n application-system \
  --set push.enabled=true \
  --set push.endpoint=wss://triage.example.com/v1/cluster-agent/ws \
  --set push.clusterName=ops \
  --set push.namespaces=ops \
  --set push.token=<bearer-token>
```

Or reference an existing Secret (key `token`):

```bash
  --set push.existingSecret=app-push-token
```

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `.../sre/application` | Controller image |
| `image.tag` | `""` (→ `appVersion`) | Image tag |
| `replicaCount` | `1` | Replicas |
| `namespace` | `""` | Restrict cache to one namespace; empty = cluster-wide |
| `syncPeriod` | `120` | Reconcile resync seconds |
| `stabilizationPeriod` | `30` | Anti-flap delay before Ready |
| `leaderElection.enabled` | `false` | Enable leader election (needs >1 replica) |
| `metrics.enabled` | `false` | Serve the metrics endpoint |
| `metrics.bindAddress` | `127.0.0.1:8080` | Metrics bind address when enabled |
| `push.enabled` | `false` | Enable push mode |
| `push.endpoint` | `""` | Triage WebSocket URL (required when enabled) |
| `push.clusterName` | `""` | Cluster name stamped into events (required when enabled) |
| `push.namespaces` | `""` | Comma-separated namespaces; empty = all |
| `push.heartbeatSeconds` | `20` | Heartbeat interval |
| `push.insecureSkipVerify` | `false` | Skip TLS verify (dev only) |
| `push.existingSecret` | `""` | Secret (key `token`) holding the bearer token |
| `push.token` | `""` | Inline token (rendered into a Secret) |
| `serviceAccount.create` | `true` | Create SA + RBAC |
| `resources` | see values | CPU/memory requests/limits |
