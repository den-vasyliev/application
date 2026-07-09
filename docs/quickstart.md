# Quick Start

## Deploy to cluster (Helm)

```bash
helm install app charts/app-controller -n application-system --create-namespace
```

Verify:
```bash
kubectl get pods -n application-system
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
