#!/usr/bin/env bash
# Copyright 2020 The Kubernetes Authors.
# SPDX-License-Identifier: Apache-2.0


set -o errexit
set -o nounset
set -o pipefail

# Detect OS and architecture. `go env` is authoritative and works on every
# platform Go supports (including Apple Silicon / linux-arm64), unlike the old
# hardcoded `arch=amd64`.
os=$(go env GOOS)
arch=$(go env GOARCH)

if [[ "$os" != "linux" && "$os" != "darwin" ]]; then
  echo "OS '$os' not supported. Aborting." >&2
  exit 1
fi

# Turn colors in this script off by setting the NO_COLOR variable in your
# environment to any value:
#
# $ NO_COLOR=1 test.sh
NO_COLOR=${NO_COLOR:-""}
if [ -z "$NO_COLOR" ]; then
  header=$'\e[1;33m'
  reset=$'\e[0m'
else
  header=''
  reset=''
fi

function header_text {
  echo "$header$*$reset"
}
