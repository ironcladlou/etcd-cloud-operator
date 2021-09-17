#!/bin/bash
set -euo pipefail

function cleanup() {
  echo "cleaning up"
  oc delete --ignore-not-found --namespace $NAMESPACE --grace-period 0 --force pods/tester
}
trap cleanup SIGINT

SCRIPT_DIR="$(cd "$( dirname "${BASH_SOURCE[0]}")" &> /dev/null && pwd)"

TEST_REGEX="${1:-.*}"

TEST_BINARY="${SCRIPT_DIR}/bin/linux_amd64/test-e2e"
if [ ! -f "$TEST_BINARY" ]; then
  echo "missing test binary at $TEST_BINARY, run build.sh to create it"
  exit 1
fi

NAMESPACE="etcd-e2e"

echo "waiting for tester pod setup"
cleanup
oc apply -f ${SCRIPT_DIR}/manifests/e2e/
oc wait --namespace $NAMESPACE --for=condition=Ready pod/tester

echo "copying build to tester pod"
oc cp -c tester $TEST_BINARY $NAMESPACE/tester:/usr/bin/test-e2e

echo "executing test '${TEST_REGEX}'"
set +e
oc exec --namespace $NAMESPACE -it -c tester tester -- /usr/bin/test-e2e -test.v -test.run "$TEST_REGEX"
set -e

cleanup
