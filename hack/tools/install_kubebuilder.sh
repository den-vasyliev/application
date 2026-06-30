#!/usr/bin/env bash
# Copyright 2020 The Kubernetes Authors.
# SPDX-License-Identifier: Apache-2.0


source ./common.sh

# Version of the envtest control-plane assets (etcd, kube-apiserver, kubectl) to
# install. Should track the k8s.io/* libraries in go.mod (currently 0.31.x).
envtest_k8s_version=${ENVTEST_K8S_VERSION:-1.31.0}

# kubebuilder CLI version (only fetched so the Makefile's $(TOOLBIN)/kubebuilder
# target has an output; the test suites use envtest, not the kubebuilder CLI).
kubebuilder_version=${KUBEBUILDER_VERSION:-4.5.2}

bindir="$PWD/bin"
mkdir -p "$bindir"

# --- envtest assets (etcd, kube-apiserver, kubectl) via setup-envtest ----------
# setup-envtest publishes native binaries for every platform Go targets,
# including darwin/arm64 and linux/arm64 — unlike the old kubebuilder 2.3.1
# tarball, which only ever shipped amd64.
if [[ ! -x "$bindir/etcd" || ! -x "$bindir/kube-apiserver" || ! -x "$bindir/kubectl" ]]; then
  header_text "Installing envtest assets (k8s ${envtest_k8s_version}, ${os}/${arch})"

  setup_envtest="$bindir/setup-envtest"
  if [[ ! -x "$setup_envtest" ]]; then
    GOBIN="$bindir" go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
  fi

  assets_dir=$("$setup_envtest" use "$envtest_k8s_version" --os "$os" --arch "$arch" -p path)
  for b in etcd kube-apiserver kubectl; do
    cp "$assets_dir/$b" "$bindir/$b"
  done
fi

# --- kubebuilder CLI -----------------------------------------------------------
if [[ ! -x "$bindir/kubebuilder" ]]; then
  header_text "Installing kubebuilder ${kubebuilder_version} (${os}/${arch})"
  curl -fsSL -o "$bindir/kubebuilder" \
    "https://github.com/kubernetes-sigs/kubebuilder/releases/download/v${kubebuilder_version}/kubebuilder_${os}_${arch}"
  chmod +x "$bindir/kubebuilder"
fi
