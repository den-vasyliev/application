# Application Passport — kube-app-manager

## 1. Identity

| Field | Value |
|---|---|
| Component | kube-app-manager (Kubernetes Application controller) |
| Repository | https://github.com/den-vasyliev/application |
| Go module | sigs.k8s.io/application |
| Registry | ghcr.io/den-vasyliev/application |
| Version (git describe) | v1.3.6 |
| Owner | den-vasyliev (den.vasyliev@gmail.com) |
| License | Apache-2.0 |

## 2. Classification

| Field | Value |
|---|---|
| Class | Kubernetes controller / operator |
| Type | CRD controller (controller-runtime, operator pattern) |
| Custom resource | `Application` (`app.k8s.io/v1beta1`), Namespaced scope |
| Capabilities | Groups related Kubernetes resources via label selector + componentKinds; aggregates component health into a single Application status; optionally sets ownerReferences on matched components |
| Model role | None (no AI/LLM component) |
| Runtime | In-cluster controller, single replica, optional leader election |
| Public accessibility | Not exposed externally; in-cluster only |
| Data access | Reads cluster resources to aggregate component health; writes limited to Application status and optional ownerReferences |
| PII | None handled by the application |
| Sensitivity | In-cluster control-plane component |

## 3. Artifact

| Field | Value |
|---|---|
| Form | OCI container image / static binary `kube-app-manager` |
| Language | Go 1.25 |
| Build tools | `go build` (Makefile), Dockerfile multi-stage, ko (`.ko.yaml`) |
| CGO | Disabled (`CGO_ENABLED=0`, statically linked) |
| Base image (Dockerfile) | `gcr.io/distroless/static:nonroot` (digest-pinned) |
| Base image (ko) | `cgr.dev/chainguard/static:latest-glibc` (digest-pinned) |
| Runtime user | `nonroot` (Dockerfile + pod/container `securityContext`) |
| Entrypoint | `/kube-app-manager` |

## 4. Privileges

RBAC granted to the controller ServiceAccount (`config/rbac/role.yaml`, deployed in
`deploy/kube-app-manager-aio.yaml` via ClusterRoleBinding).

| API groups | Resources | Verbs |
|---|---|---|
| `*` | `*` | get, list, watch, update, patch |
| `app.k8s.io` | `applications` | create, delete, get, list, patch, update, watch |
| `app.k8s.io` | `applications/status` | get, patch, update |

Leader-election role (namespaced) and the kube-rbac-proxy sidecar role
(`authentication.k8s.io/tokenreviews` create, `authorization.k8s.io/subjectaccessreviews`
create) are bound separately.

Dynamic component watches are registered at runtime per `spec.componentKinds` GVK, but
scoped in code to a `workloadKinds` allow-list (Deployment, StatefulSet, ReplicaSet,
DaemonSet, Pod, Service, PVC, PodDisruptionBudget, ReplicationController, Job, CronJob,
Argo Rollout). Secret / ConfigMap / ServiceAccount kinds are explicitly excluded from
real-time watches.

## 5. Network Surface

| Port | Listener | Component | Deployed bind | Auth state |
|---|---|---|---|---|
| 8080 | HTTP metrics | controller manager (`--metrics-addr`) | `127.0.0.1:8080` (loopback default) | Fronted by the rbac-proxy |
| 8443 | HTTPS | kube-rbac-proxy sidecar (`gcr.io/kubebuilder/kube-rbac-proxy:v0.4.1`) | `0.0.0.0:8443` | RBAC via TokenReview + SubjectAccessReview |
| 443 → 8443 | Service | metrics Service | cluster-internal | (fronts the proxy) |

The controller does not serve health/readiness probe endpoints in the deployed manifest.
No webhook server is configured.

## 6. Dependency Vulnerabilities

SBOM: CycloneDX 1.6 (syft), Go ecosystem, 63 components. Scan: trivy (`go.mod`).

| Ecosystem | Critical | High | Medium | Low | Unknown |
|---|---|---|---|---|---|
| Go (go.mod) | 0 | 0 | 0 | 0 | 0 |

No known vulnerabilities.
