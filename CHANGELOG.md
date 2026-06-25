# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [1.2.1] - 2026-06-25

### Fixed

- **The actual prod scale-up flap: `deploymentStatus` gated "serving" on the
  `Available` *condition* alone, ignoring `availableReplicas`.** During an HPA scale-up
  kube updates `status.availableReplicas` first and writes the `Available` condition a
  beat later. In that window the Deployment had replicas serving (`availableReplicas>0`)
  but no `Available` condition yet, so the controller read it as not-available and
  reported `InProgress` — flapping the Application to degraded on every scale-up. This is
  the flap that survived 1.2.0 (1.2.0 fixed the replica-count and generation-skew gates
  but still trusted the condition exclusively). A Deployment is now treated as serving if
  the `Available` condition is `True` **or** `availableReplicas>0` and the condition is
  not explicitly `False`; a genuine `Available=False` still reports `InProgress`.
  The other workload kinds (StatefulSet, ReplicaSet, ReplicationController, DaemonSet)
  already decide on replica counters rather than the condition, so Deployment was the
  only affected kind.

  Crucially, this is covered by a new **envtest** spec (`scaleup_envtest_test.go`) that
  drives a real Deployment through a real apiserver with the `Available` condition
  absent — the prod state. It fails on the pre-fix code and passes on the fix. The prior
  unit tests missed the bug because they always set an `Available` condition explicitly,
  so the real "condition-not-yet-published" window was never exercised.

## [1.2.0] - 2026-06-25

### Fixed

- **Workload readiness no longer flaps on HPA scale-up.** Scalable workloads are now
  reported `Ready` when they are *serving*, not when they have reached their full
  desired replica count. Previously an HPA scale-up (e.g. 2→3) flipped the workload —
  and therefore the Application's `Ready` condition — to `InProgress`/degraded for the
  ~90s the new pod needed to pass its readiness probe, which monitoring read as a false
  incident. Applies to Deployment, StatefulSet, ReplicaSet, ReplicationController, and
  DaemonSet (node-join). Scaled-to-zero still resolves to `Ready`, and genuine failures
  (nothing available / `ReplicaFailure` / unavailable pods) still report `InProgress`.
  See [ADR-0003](doc/adr/0003-scale-up-readiness-flap.md) (Accepted). _(live on image tag
  `25171c3`; `main` rebuilt as `1f3ab67`)_

- **Scale-up flap, part 2: removed the `ObservedGeneration` gate from the readiness
  predicate.** The first fix (`25171c3`) still required `status.observedGeneration ==
  metadata.generation` to report `Ready`. An HPA scale-up bumps `generation` instantly
  while the workload controller writes `observedGeneration` a beat later; in that window
  `Available` was still `True` but the generation clause was false, so the workload was
  reported `InProgress` and the Application was paged as degraded anyway (observed in production
  2026-06-25 on a build containing `25171c3`). The clause is removed from all five kinds;
  genuine failure is still caught by `Available=False` / `ReplicaFailure` / unavailable
  pods. Regression specs now reproduce the generation-skew window for every kind.

- **Argo Rollouts no longer flap on scale-up / canary steps.** `rolloutStatus` mapped
  `status.phase: Progressing` and `Paused` to `InProgress`, so a Rollout scaling up (HPA
  or replica bump) or stepping through a canary/blue-green rollout degraded its
  Application even while it kept serving (`Available` condition `True`) — the same false
  positive as the Deployment case, on the kind production actually runs on (~27 Rollouts).
  `Progressing` and `Paused` now map to `Ready`; only `Degraded`/`Error` report
  `InProgress`. Additionally, a **scaled-to-zero Rollout** (`spec.replicas=0`) is now
  `Ready` regardless of phase, so a parked Rollout with a `Degraded`/`InvalidSpec` phase
  (e.g. `example-parked-rollout`) doesn't degrade its app. Adds the first `rolloutStatus` unit
  specs (the kind previously had none). See [ADR-0003](doc/adr/0003-scale-up-readiness-flap.md).

### Changed

- Test suites are envtest-only: removed the `UseExistingCluster` option from both the
  `controllers` and `e2e` suites and added a `127.0.0.1` host guard so no test can run
  against a real cluster / the current kubeconfig.

### Internal

- Repaired the `controllers` envtest suite so it runs: `CreateController` now applies a
  unique `.Named(name)` (previously the name argument was unused, causing a
  controller-name collision on the second spec); `StartTestManager` no longer asserts on
  the manager's shutdown error; and the stale ownerReference spec was rewritten to assert
  the controller does **not** mutate owner references (mutation was disabled in an earlier
  change).
- Added `controllers/status_test.go` with unit coverage for all five scalable workload
  kinds.
