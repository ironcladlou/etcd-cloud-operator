FROM quay.io/openshift/origin-base:4.9

# 4.9.0
FROM quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:9dcdfbea3f60dd99950959811bec6b9c6cffadf6a65de339ae1d65083300bcc1 as etcd

COPY --from=etcd /usr/bin/etcdctl /usr/bin/etcdctl

COPY . .

RUN hypershift/build.sh

COPY hypershift/bin/${GOOS}_${GOARCH}/etcd-cloud-operator /usr/bin/etcd-cloud-operator
COPY hypershift/bin/${GOOS}_${GOARCH}/test-e2e /usr/bin/test-e2e

ENTRYPOINT /usr/bin/etcd-cloud-operator
