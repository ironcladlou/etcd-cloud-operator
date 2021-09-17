#!/bin/bash
set -euo pipefail

TAG=${TAG:-quay.io/dmace/etcd-cloud-operator:latest}
OCP_DOCKER_CONFIG=${OCP_DOCKER_CONFIG:-$HOME/.docker/configs/quay-openshift-release-dev}

SCRIPT_DIR="$(cd "$( dirname "${BASH_SOURCE[0]}")" &> /dev/null && pwd)"

export GOOS=linux GOARCH=amd64
BIN_DIR="${SCRIPT_DIR}/bin/${GOOS}_${GOARCH}"

docker --config $OCP_DOCKER_CONFIG build -f $SCRIPT_DIR/Dockerfile.local -t $TAG $BIN_DIR
