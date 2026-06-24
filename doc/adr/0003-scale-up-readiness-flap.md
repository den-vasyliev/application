# ADR-0003: Don't Flap Workload Readiness on HPA Scale-Up

- **Date:** 2026-06-24
- **Status:** Proposed (in test on image tag `25171c3`)
- **Commit:** `25171c3`

## Context

An Application was being paged as **degraded** every time its HPA scaled a
Deployment up. Example status during an autoscale event:

```
Components:
  Kind: Deployment   Name: example-service   Status: InProgress
  Kind: Service      ...                                          Status: Ready
  Kind: Ingress      ...                                          Status: Ready
Components Ready: 2/3
```

### Root Cause

The per-kind status functions in `controllers/status.go` required the workload to
be at its **full desired replica count** before reporting `Ready` — e.g.
`deploymentStatus` required `ReadyReplicas == AvailableReplicas == spec.replicas`,
and `stsStatus` / `replicasetStatus` / `replicationControllerStatus` likewise.

When the HPA scales 2→3, `spec.replicas` jumps to 3 immediately, but the new pod
needs its readiness-probe window (observed: up to **90s**) before it counts as
ready. During that window `ReadyReplicas (2) != spec.replicas (3)`, so the function
returned `InProgress`. That flipped the Application's `Ready` condition to `False`
(`2/3`), and monitoring read the condition transition as a degraded incident — even
though the existing replicas were serving fine and the app was only *adding*
capacity.

This is a false positive: **scaling up is not degradation.** Kubernetes itself does
not consider the workload unavailable during a healthy scale-up (a Deployment's
`Available` condition stays `True`).

Note: this controller emits **no Kubernetes events** (there is no `Recorder` in the
code). The signal monitoring keys on is the Application's `Ready` condition
transitioning to `False`.

## Decision

Report scalable workloads as `Ready` when they are **serving** (their currently
running replicas are healthy), not when they have reached the full *desired* count.
Each kind uses the signal Kubernetes already publishes for "available":

| Kind | Ready when | Notes |
|------|-----------|-------|
| **Deployment** | `Available` condition `True` and no `ReplicaFailure` | `Available` respects `maxUnavailable`; stays True through a healthy scale-up |
| **StatefulSet** | `ReadyReplicas == CurrentReplicas` (and `CurrentReplicas > 0`) | STS has no `Available` condition; `CurrentReplicas` is what the controller has actually rolled, so this means "all running pods are ready" |
| **ReplicaSet** | `AvailableReplicas > 0` and no `ReplicaFailure` | |
| **ReplicationController** | `AvailableReplicas > 0` | no conditions in RC status |
| **DaemonSet** | `NumberUnavailable == 0` | the DS controller's own counter respects `maxUnavailable`, so a node join doesn't flap |

All keep `ObservedGeneration == Generation` (don't trust stale status), preserve the
**scaled-to-zero → Ready** case, and still report `InProgress` on genuine failure
(nothing available / `ReplicaFailure=True` / unavailable pods).

### Behavior matrix

| Scenario | Before | After |
|----------|--------|-------|
| Scale-up 2→3, existing pods healthy | `InProgress` (false incident) | **Ready** |
| All replicas down / can't meet minAvailable | `InProgress` | `InProgress` |
| `ReplicaFailure=True` | `InProgress` | `InProgress` |
| Scaled to zero | `Ready` | `Ready` |

## Alternatives Considered

- **Tune the controller flags** (`--sync-period`, `--stabilization-period`): these
  affect *when* status is re-read and flap-damping on the Ready→NotReady→Ready edge,
  but the readiness *predicate* was the bug — the workload genuinely reported
  `InProgress`. Tuning would only shorten or mask the false-degraded window, not
  remove it.
- **Move the 90s tolerance into a `startupProbe`** on the workload: a good
  complementary fix (it stops kubelet `Unhealthy` events during boot), but it lives
  in each app's pod spec, not in this controller, and does not change that a
  scale-up legitimately lags `ReadyReplicas`.

## Consequences

- An Application no longer transitions to `NotReady`/degraded while an HPA adds
  capacity to any scalable workload (Deployment, StatefulSet, ReplicaSet,
  ReplicationController, DaemonSet node-join).
- Slightly less strict: an Application reports `Ready` while a workload is below its
  full desired count, as long as it is serving and not failing. This matches
  Kubernetes' own "Available" semantics and is the intended trade-off.
- No change to the public API or CRD schema.

## Tests

`controllers/status_test.go` — unit specs for all five kinds, each covering a
scale-up regression, a broken case, and scaled-to-zero. The pre-existing
`controllers` envtest suite was also repaired so it runs (controller-name collision,
manager-shutdown assertion, and a stale ownerReference spec that tested removed
owner-ref mutation).

## Deployment

```
crane copy ghcr.io/den-vasyliev/application:25171c3 \
  REGISTRY/application:25171c3

kubectl set image deployment/kube-app-manager-controller \
  kube-app-manager=REGISTRY/application:25171c3 \
  -n application-system
```
