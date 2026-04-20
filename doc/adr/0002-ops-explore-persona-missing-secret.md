# ADR-0002: Ops explore-persona Rollout Degraded — Missing Secret

- **Date:** 2026-04-20
- **Status:** Resolved
- **Cluster:** ops / namespace: ops

## Context

`ops-ecosystem-explore-persona` Application showed `3/4 componentsReady` with
the Argo Rollout component in `InProgress` state.

The Rollout was `Degraded` with `ProgressDeadlineExceeded` since 2026-04-16.
The new ReplicaSet `67cbfc6969` had one pod stuck in `ContainerCreating` for
90+ minutes.

## Root Cause

Pod events showed:

```
MountVolume.SetUp failed for volume "api-key-ecosystem-market-updates":
secret "service-api-key-ecosystem-market-updates" not found
```

The Rollout spec was updated to mount a new secret
`service-api-key-ecosystem-market-updates` but the secret was never created in
the `ops` namespace. A `VaultStaticSecret` existed in `dev` namespace but:

1. The Vault Secrets Operator was in `ImagePullBackOff` (broken), so no secrets
   were being synced from Vault.
2. The Vault path `ecosystem/ops/service-api-keys/ecosystem-market-updates` did
   not exist — only the `dev` path was populated.

## Resolution

The secret data was sourced from the existing `market-updates` Opaque secret in
`ops` namespace (synced 123 days ago when the operator was healthy), which
contained the same keys (`api-key.txt`,
`market-updates-notification-api-key.txt`).

Created the secret manually:

```bash
kubectl create secret generic service-api-key-ecosystem-market-updates -n ops \
  --from-literal=api-key.txt=<value from market-updates> \
  --from-literal=market-updates-notification-api-key.txt=<value from market-updates>
```

Also created a `VaultStaticSecret` in `ops` pointing to the `dev` Vault path as
a placeholder until the ops Vault path is populated:

```yaml
apiVersion: secrets.hashicorp.com/v1beta1
kind: VaultStaticSecret
metadata:
  name: service-api-key-ecosystem-market-updates
  namespace: ops
spec:
  mount: secret
  namespace: dev
  path: ecosystem/dev/service-api-keys/ecosystem-market-updates
  destination:
    create: true
    name: service-api-key-ecosystem-market-updates
  type: kv-v2
  refreshAfter: 300s
```

## Follow-up Actions

- [ ] Fix Vault Secrets Operator `ImagePullBackOff` in `vault` namespace
- [ ] Create Vault secret at `ecosystem/ops/service-api-keys/ecosystem-market-updates`
      and update the `VaultStaticSecret` to point to the ops path
- [ ] Investigate why the Rollout spec was updated without the secret being
      provisioned first
