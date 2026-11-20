#!/usr/bin/env bash
# Runs the tests against a real kube-apiserver and etcd (envtest).
# The first run downloads the Kubernetes 1.37 test binaries (about 180 MB)
# into ENVTEST_DIR (default: ./bin/envtest).
set -euo pipefail
cd "$(dirname "$0")/.."
# Absolute: go test runs each package in its own directory, so a relative
# KUBEBUILDER_ASSETS would point nowhere (the first CI run failed this way).
dir="${ENVTEST_DIR:-bin/envtest}"
case "$dir" in /* | [A-Za-z]:*) ;; *) dir="$PWD/$dir" ;; esac
export KUBEBUILDER_ASSETS="${KUBEBUILDER_ASSETS:-$(go tool setup-envtest use 1.37.0 -p path --bin-dir "$dir")}"
case "$KUBEBUILDER_ASSETS" in /* | [A-Za-z]:*) ;; *) KUBEBUILDER_ASSETS="$PWD/$KUBEBUILDER_ASSETS" ;; esac
go vet ./...
go test -count=1 "$@" ./...
