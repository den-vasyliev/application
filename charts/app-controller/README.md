# app-controller Helm chart

Installs the Kubernetes Application controller (`app.k8s.io/v1beta1`) with optional
**push mode** — streaming Application inventory + Kubernetes Warning events to a
triage agent over an outbound WebSocket for clusters with no inbound API access
(see [ADR-0005](../../docs/adr/0005-outbound-push-mode.md)).

No `kube-rbac-proxy` sidecar and metrics disabled by default — the two legacy
kubebuilder-scaffold extras. Enable metrics explicitly if you scrape them.

## Install

```bash
helm install app charts/app-controller -n triage --create-namespace
```

The Application CRD ships in `crds/` and is installed automatically on first
install. (Helm does not upgrade CRDs — apply CRD changes manually.)

## Push mode

```bash
helm upgrade --install app charts/app-controller -n triage \
  --set push.enabled=true \
  --set push.endpoint=wss://triage.example.com/v1/cluster-agent/ws \
  --set push.clusterName=ops \
  --set push.namespaces=ops \
  --set push.tenant=ops \
  --set push.token=<hmac-signing-key>
```

Or reference an existing Secret (key `token`):

```bash
  --set push.existingSecret=app-push-token
```

## Log-based metrics

Ships a `triage-fluentbit` DaemonSet that tails container logs, counts error/warn
lines per (namespace, service) via Fluent Bit's `log_to_metrics` filter, and exposes
them as Prometheus counters. The controller scrapes that DaemonSet's pods, computes
per-interval deltas, and forwards services whose error delta crosses a threshold to
triage as `log_metrics` frames over the same push connection — see
[ADR-0006](../../docs/adr/0006-log-based-metrics.md). Requires `push.enabled=true`.

```bash
helm upgrade --install app charts/app-controller -n triage --reuse-values \
  --set fluentbit.enabled=true \
  --set logMetrics.enabled=true
```

Both toggles are independent: a fleet that already runs its own Fluent Bit can set
only `logMetrics.enabled=true` and point `logMetrics.serviceName`/`logMetrics.port`
at its existing exporter Service.

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `.../sre/application` | Controller image |
| `image.tag` | `""` (→ `appVersion`) | Image tag |
| `replicaCount` | `1` | Replicas |
| `namespace` | `""` | Restrict cache to one namespace; empty = cluster-wide |
| `syncPeriod` | `120` | Reconcile resync seconds |
| `stabilizationPeriod` | `30` | Anti-flap delay before Ready |
| `concurrentReconciles` | `4` | Max Applications reconciled in parallel |
| `leaderElection.enabled` | `false` | Enable leader election (needs >1 replica) |
| `metrics.enabled` | `false` | Serve the metrics endpoint |
| `metrics.bindAddress` | `127.0.0.1:8080` | Metrics bind address when enabled |
| `push.enabled` | `false` | Enable push mode |
| `push.endpoint` | `""` | Triage WebSocket URL (required when enabled) |
| `push.clusterName` | `""` | Cluster name stamped into events (required when enabled) |
| `push.tenant` | `""` | Tenant selecting the triage service graph; bound into the signed handshake (required when enabled) |
| `push.namespaces` | `""` | Comma-separated namespaces; empty = all |
| `push.heartbeatSeconds` | `20` | Heartbeat interval |
| `push.insecureSkipVerify` | `false` | Skip TLS cert verify for `wss://` (dev only) |
| `push.allowPlaintext` | `false` | Allow plaintext `ws://` endpoint (token unencrypted) |
| `push.existingSecret` | `""` | Secret (key `token`) holding the HMAC signing key |
| `push.token` | `""` | Inline token (rendered into a Secret) |
| `logMetrics.enabled` | `false` | Enable the log-metrics collector (requires `push.enabled`) |
| `logMetrics.serviceName` | `triage-fluentbit` | Service fronting the Fluent Bit exporter pods |
| `logMetrics.port` | `2021` | Fluent Bit `prometheus_exporter` port |
| `logMetrics.intervalSeconds` | `60` | Scrape + gate-evaluation interval |
| `logMetrics.errorThreshold` | `10` | Minimum error-count delta per interval to report a service |
| `logMetrics.errorMetric` / `warnMetric` / `totalMetric` | `log_metric_counter_log_{errors,warns,lines}_total` | Prometheus counter family names Fluent Bit emits |
| `logMetrics.namespaceLabel` / `serviceLabel` / `serviceLabelFallback` | `namespace` / `service` / `service_fallback` | Label keys identifying each sample |
| `fluentbit.enabled` | `false` | Deploy the `triage-fluentbit` DaemonSet + ConfigMap + Service + RBAC |
| `fluentbit.image.repository` / `tag` | `fluent/fluent-bit` / `5.0.9` | Fluent Bit image |
| `fluentbit.port` | `2021` | Exporter port (must match `logMetrics.port` if scraping this DaemonSet) |
| `fluentbit.excludeNamespaces` | `[kube-system, kube-node-lease, kube-public]` | Namespaces excluded from tailing |
| `fluentbit.pipeline.errorRegex` / `warnRegex` | see values.yaml | Regex matched against each log line to classify it |
| `fluentbit.pipeline.serviceLabel` / `serviceLabelFallback` | `app.kubernetes.io/name` / `app` | Pod labels read as the service identity |
| `serviceAccount.create` | `true` | Create SA + RBAC |
| `resources` | see values | CPU/memory requests/limits |
