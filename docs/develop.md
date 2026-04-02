# Development Guide

## Prerequisites

- [Go](https://golang.org/dl/) v1.24+
- [ko](https://ko.build) — container image builds
- [kubectl](https://kubernetes.io/docs/tasks/tools/) + cluster access (optional, for deploy targets)

Install build tools:
```bash
make install-tools
```

## Workflow

### Build

```bash
make bin/kube-app-manager    # binary
make ko-image                # OCI image (local layout, no push)
make ko-push                 # build and push to $KO_DOCKER_REPO
```

### Test (no cluster required — uses envtest)

```bash
make test        # unit tests
make e2e-test    # e2e tests
```

Run a single test with Ginkgo focus:
```bash
go test -v ./controllers/... -run "TestAPIs" --ginkgo.focus="<test name>"
```

### Code quality

```bash
make fmt         # go fmt
make vet         # go vet
make lint        # golangci-lint
```

### Manifests / codegen

```bash
make manifests   # regenerate CRD/RBAC manifests (controller-gen)
make generate    # regenerate deepcopy methods
```

## Deploy to cluster

```bash
make deploy CONTROLLER_IMG=<image>
make undeploy
```

## Using the Application CRD

See [api.md](api.md) for field reference and [examples/wordpress](examples/wordpress) for a working example.

### Programmatic access

```go
kubeClient, err := client.New(config, client.Options{})

object := &applicationsv1beta1.Application{}
err = kubeClient.Get(context.TODO(), types.NamespacedName{
    Namespace: "default",
    Name:      "my-app",
}, object)
```
