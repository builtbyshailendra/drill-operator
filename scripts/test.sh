#!/usr/bin/env bash
# Runs the tests against a real kube-apiserver and etcd (envtest).
# The first run downloads the Kubernetes 1.37 test binaries (about 180 MB)
# into ENVTEST_DIR (default: ./bin/envtest).
set -euo pipefail
cd "$(dirname "$0")/.."
export KUBEBUILDER_ASSETS="${KUBEBUILDER_ASSETS:-$(go tool setup-envtest use 1.37.0 -p path --bin-dir "${ENVTEST_DIR:-bin/envtest}")}"
go vet ./...
go test -count=1 "$@" ./...
