#!/bin/bash
set -euo pipefail

TAG=${TAG:-quay.io/dmace/etcd-cloud-operator:latest}
PUSH_DOCKER_CONFIG=${PUSH_DOCKER_CONFIG:-$HOME/.docker/configs/quay-dmace}

docker --config $PUSH_DOCKER_CONFIG push $TAG
