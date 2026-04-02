# Quick Start

## Deploy to cluster

```bash
kubectl apply -f https://raw.githubusercontent.com/den-vasyliev/application/master/deploy/kube-app-manager-aio.yaml
```

Verify:
```bash
kubectl get pods -n application-system
kubectl get crd applications.app.k8s.io
```

## Example Application

```bash
kubectl apply -f docs/examples/wordpress/
kubectl get application wordpress-01
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
make bin/kube-app-manager
./bin/kube-app-manager --help
```

## Build and push image

```bash
export KO_DOCKER_REPO=ghcr.io/<your-org>/application
make ko-push
```

## Deploy to cluster

```bash
make deploy CONTROLLER_IMG=<image>
make undeploy
```
