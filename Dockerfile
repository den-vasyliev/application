# Copyright 2020 The Kubernetes Authors.
# SPDX-License-Identifier: Apache-2.0

FROM golang:1.24 AS builder
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

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/kube-app-manager .
USER nonroot:nonroot
ENTRYPOINT ["/kube-app-manager"]
