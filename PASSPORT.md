# Application Passport — app-controller

## 1. Identity

| Field | Value |
|---|---|
| Component | app-controller (Kubernetes Application controller) |
| Description | Groups related Kubernetes resources into an `Application` and aggregates their health; optionally pushes Application inventory + Warning events to a triage agent over an outbound WebSocket |
| Repository | https://github.com/den-vasyliev/application |
| Go module | sigs.k8s.io/application |
| Registry | ghcr.io/den-vasyliev/application (also mirrored to internal Artifact Registry) |
| Version | v1.4.5 |
| Owner | den-vasyliev (den.vasyliev@gmail.com) |
| License | Apache-2.0 |
| Audit status | No CVEs (0 across all severities); 0 open HIGH/CRITICAL — refreshed 2026-07-14 |
| Code review | Passed 2026-07-14 (full `controllers/` + `push/` + `main.go` review; all findings fixed in v1.4.5, see AUDIT.md) |

## 2. Classification

| Field | Value |
|---|---|
| Class | Kubernetes controller / operator |
| Type | CRD controller (controller-runtime, operator pattern) |
| Custom resource | `Application` (`app.k8s.io/v1beta1`), Namespaced scope |
| Capabilities | Groups Kubernetes resources via label selector (matchLabels + matchExpressions) + componentKinds; aggregates component health into Application status; optionally streams Applications + Kubernetes Warning events to a triage agent (push mode) |
| Model role | None (no AI/LLM component) |
| Runtime | In-cluster controller, single replica, optional leader election; `--concurrent-reconciles` workers (default 4) |
| Public accessibility | Not exposed externally; outbound-only when push mode is enabled |
| Data access | Reads cluster resources to aggregate component health (served from informer cache, `managedFields` stripped); writes limited to Application status. Push mode reads Applications + Warning events and sends them off-cluster |
| PII | None handled by the controller |
| Sensitivity | In-cluster control-plane component |

## 3. Artifact

| Field | Value |
|---|---|
| Form | OCI container image / static binary `app-controller` |
| Language | Go 1.26 |
| Key frameworks | controller-runtime v0.24.1, k8s.io/* v0.36.0 |
| Build tools | `go build` (Makefile), Dockerfile multi-stage, ko (`.ko.yaml`) |
| CGO | Disabled (`CGO_ENABLED=0`, statically linked) |
| Base image (Dockerfile) | `gcr.io/distroless/static:nonroot` @ `sha256:963fa6c5…df5240` (digest-pinned) |
| Base image (ko) | `cgr.dev/chainguard/static:latest-glibc` @ `sha256:77d8b892…cb75b` (digest-pinned) |
| Runtime user | `nonroot:nonroot` |
| Entrypoint | `/app-controller` |
| Deploy | Helm chart `charts/app-controller` (bundles CRD; no kube-rbac-proxy sidecar) |

## 4. Egress (push mode)

| Destination | Purpose | Transport | Auth | Note |
|---|---|---|---|---|
| `--push-endpoint` (triage agent WS) | Stream Application inventory + deltas + Kubernetes Warning events | WebSocket (`wss://`; plaintext `ws://` requires explicit `--push-allow-plaintext`) | Pre-upgrade: `Authorization: Bearer <handshake signature>` + `X-Triage-Tenant`/`-Cluster`/`-Ts` headers; post-upgrade: HMAC-SHA256-signed hello frame (tenant, cluster, timestamp) — the signing key itself never travels on the wire | Outbound only; disabled unless `--push-endpoint` is set; `--tenant` and `--cluster-name` required |

## 5. Privileges

| Scope | API groups | Resources | Verbs |
|---|---|---|---|
| ClusterRole (always) | `*` | `*` | get, list, watch |
| ClusterRole (always) | `app.k8s.io` | `applications` | get, list, watch, create, update, patch, delete |
| ClusterRole (always) | `app.k8s.io` | `applications/status` | get, update, patch |
| Role (leaderElection only) | `coordination.k8s.io` | `leases` | get, list, watch, create, update, patch, delete |
| Role (leaderElection only) | `""` | `events` | create, patch |

| Field | Value |
|---|---|
| ServiceAccount | Created by the chart (`serviceAccount.create: true`); dedicated per release |
| Source RBAC marker | Matches the chart: read-only `get;list;watch` on `*/*` (stale `update;patch` marker removed in v1.4.5) |
| Dynamic watches | Registered at runtime per `spec.componentKinds` GVK; workload kinds only: Deployment, StatefulSet, DaemonSet, ReplicaSet, ReplicationController, Job, CronJob, Argo Rollout, Gateway, HTTPRoute, kagent Agent/ModelConfig |
| Excluded from real-time watches | Secret / ConfigMap / ServiceAccount and other non-workload kinds |
| Event informer (push mode) | Scoped server-side to `type=Warning` via field selector |
| Token mount (push mode) | k8s Secret `token` key mounted read-only at `/etc/push` |

## 6. Network Surface

| Port | Listener | Deployed bind | Auth state |
|---|---|---|---|
| 8080 | HTTP metrics (`--metrics-addr`) | Disabled by default (`--metrics-addr=0`); when enabled binds `127.0.0.1:8080` | No sidecar; plain endpoint on loopback |

| Field | Value |
|---|---|
| Inbound listeners | None in push mode (outbound WebSocket only) |
| Webhook server | None configured |
| Health/readiness endpoints | None served in the deployed manifest |
| kube-rbac-proxy sidecar | Removed (was present in the legacy kustomize deploy) |

## 7. Dependency Vulnerabilities

| Ecosystem | Components (SBOM) | Critical | High | Medium | Low | Unknown | Scanned |
|---|---|---|---|---|---|---|---|
| Go (go.mod) | 66 (CycloneDX 1.6, syft) | 0 | 0 | 0 | 0 | 0 | 2026-07-14 (trivy) |
