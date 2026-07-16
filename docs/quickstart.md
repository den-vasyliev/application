# Quick Start

## Deploy to cluster (Helm)

```bash
helm install app charts/app-controller -n triage --create-namespace
```

Verify:
```bash
kubectl get pods -n triage
kubectl get crd applications.app.k8s.io
```

## Example Application

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
    - group: apps
      kind: Deployment
    - group: ""
      kind: Service
```

## Push mode (remote agent)

If this cluster's API isn't reachable from wherever you monitor it (behind a
firewall/NAT), the controller can dial **out** to a triage agent over a
WebSocket and stream Application status + Kubernetes Warning events instead
([ADR-0005](adr/0005-outbound-push-mode.md)). It's opt-in — off unless
`push.enabled=true`.

Auth is HMAC, not a bearer token: `push.token` is a per-tenant **signing key**
that's never sent on the wire. The agent uses it to sign a handshake over
`(tenant, clusterName, timestamp)`; the triage receiver verifies that
signature with its own copy of the same key, which must be registered under
the exact same `clusterName` beforehand or the handshake is rejected as
"unknown cluster".

```bash
helm upgrade --install app charts/app-controller -n triage \
  --set push.enabled=true \
  --set push.endpoint=wss://<host>/v1/cluster-agent/ws \
  --set push.clusterName=<cluster> \
  --set push.tenant=<tenant> \
  --set push.namespaces=<ns1>\,<ns2> \
  --set push.token=<hmac-signing-key>   # openssl rand -base64 32
```

`push.clusterName` and `push.tenant` are both required once `push.enabled` is
true. `push.namespaces` scopes which namespaces get pushed — empty pushes
**all** namespaces, so set it explicitly unless that's intended. Prefer
`push.existingSecret` (a Secret with a `token` key) over the inline
`push.token` outside of local testing. Full option list in the
[chart README](../charts/app-controller/README.md#push-mode) and
[values.yaml](../charts/app-controller/values.yaml).

# Dev Quick Start

## Clone

```bash
git clone https://github.com/den-vasyliev/application.git
cd application
```

## Run tests (no cluster required)

```bash
make test        # unit tests
make e2e-test    # e2e tests
```

## Build binary

```bash
make bin/app-controller
./bin/app-controller --help
```

## Build and push image

```bash
export KO_DOCKER_REPO=ghcr.io/<your-org>/app-controller
make ko-push
```

## Deploy to cluster

```bash
make deploy      # helm upgrade --install
make undeploy
```
