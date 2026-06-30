# Copyright 2020 The Kubernetes Authors.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.25 AS builder
WORKDIR /workspace

ARG goproxy=https://proxy.golang.org
ENV GOPROXY=$goproxy

COPY go.mod go.sum ./
RUN go mod download

COPY main.go main.go
COPY api/ api/
COPY controllers/ controllers/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -a -ldflags '-extldflags "-static" -s -w' \
    -o kube-app-manager main.go

# Pinned by digest for reproducible builds; tag (gcr.io/distroless/static:nonroot) kept for readability.
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
WORKDIR /
COPY --from=builder /workspace/kube-app-manager .
USER nonroot:nonroot
ENTRYPOINT ["/kube-app-manager"]
