# Security Audit — kube-app-manager

_Audited 2026-06-30 against v1.3.6. Closure summary; see `PASSPORT.md` for component facts._

## Result

**All findings resolved or accepted — 0 open.** No known dependency vulnerabilities.

| Area | Status |
|---|---|
| Dependency vulnerabilities | Resolved — 0 reported by trivy |
| Container / pod hardening | Resolved |
| Build reproducibility | Resolved |
| Network surface defaults | Resolved |
| Release / artifact currency | Resolved |
| RBAC scope | Reviewed and accepted as appropriate to the controller's design |

## Scope

- Go dependency supply chain (`go.mod`) — SBOM (syft) + vulnerability scan (trivy).
- RBAC / ServiceAccount privileges (`config/rbac/`, `deploy/kube-app-manager-aio.yaml`).
- Container artifact (Dockerfile, `.ko.yaml`) and deployed pod spec.
- Network surface (metrics, sidecar, Service).
- Dynamic component-watch scoping.

Not covered: live-cluster posture (admission control, NetworkPolicy, runtime config),
the built image's full OS-layer SBOM, and third-party sidecar image internals.

## Remediation

Resolved in the hardening change set (see `CHANGELOG.md`):

- **Dependencies** updated to current releases; Go toolchain raised to 1.25.
  Vulnerability scan: **0 findings**.
- **Pod/container `securityContext`** added to the deployed manifest (`runAsNonRoot`,
  non-root uid/gid, `RuntimeDefault` seccomp, no privilege escalation, read-only root
  filesystem, all capabilities dropped).
- **Base images** pinned by digest for reproducible builds.
- **Defaults** hardened: metrics bound to loopback.
- **Artifact currency**: deployed image and `VERSION` aligned with the released source;
  sidecar log verbosity reduced.

The controller's RBAC scope was reviewed and kept deliberately: it reconciles
user-selected component kinds that cannot be enumerated ahead of time, so a broad read
grant is intrinsic to the abstraction.

## Verification

- `trivy fs go.mod` — 0 vulnerabilities.
- `make test` — controller, API, and e2e suites pass (envtest).
- `go build ./...` and `go vet ./...` — clean.
