#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$( dirname "${BASH_SOURCE[0]}")" &> /dev/null && pwd)"

export GOOS=linux GOARCH=amd64 CGO_ENABLED=0

BIN_DIR="${SCRIPT_DIR}/bin/${GOOS}_${GOARCH}"
mkdir -p $BIN_DIR

echo "building"
go mod download
go build -gcflags=all='-N -l' -o $BIN_DIR/etcd-cloud-operator ./cmd/operator

pushd hypershift >/dev/null
  go test -c -o $BIN_DIR/test-e2e ./e2e
popd >/dev/null

echo "finished build"
