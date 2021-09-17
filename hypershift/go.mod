module github.com/openshift/etcd-cloud-operator

go 1.16

require (
	github.com/prometheus/client_golang v1.11.0
	github.com/quentin-m/etcd-cloud-operator v0.0.0-20210913210441-e176d36eeffd
	go.etcd.io/etcd/api/v3 v3.5.0
	go.etcd.io/etcd/client/pkg/v3 v3.5.0
	go.etcd.io/etcd/client/v3 v3.5.0
	go.etcd.io/etcd/etcdutl/v3 v3.5.0
	go.etcd.io/etcd/server/v3 v3.5.0
	go.uber.org/zap v1.18.1
	golang.org/x/crypto v0.0.0-20210220033148-5ea612d1eb83
	golang.org/x/time v0.0.0-20210723032227-1f47c861a9ac
	google.golang.org/grpc v1.38.0
	gopkg.in/yaml.v2 v2.4.0
	k8s.io/api v0.21.4
	k8s.io/apimachinery v0.21.4
	k8s.io/utils v0.0.0-20210802155522-efc7438f0176
	moul.io/zapfilter v1.6.1
	sigs.k8s.io/controller-runtime v0.9.6
	sigs.k8s.io/yaml v1.2.0
)

replace (
	github.com/quentin-m/etcd-cloud-operator => ../

	go.etcd.io/etcd/api/v3 => github.com/openshift/etcd/api/v3 v3.5.1-0.20210824132657-5c1feaf09d5f
	go.etcd.io/etcd/client/pkg/v3 => github.com/openshift/etcd/client/pkg/v3 v3.5.1-0.20210824132657-5c1feaf09d5f
	go.etcd.io/etcd/client/v3 => github.com/openshift/etcd/client/v3 v3.5.1-0.20210824132657-5c1feaf09d5f
	go.etcd.io/etcd/etcdutl/v3 => github.com/openshift/etcd/etcdutl/v3 v3.5.1-0.20210824132657-5c1feaf09d5f
	go.etcd.io/etcd/server/v3 => github.com/openshift/etcd/server/v3 v3.5.1-0.20210824132657-5c1feaf09d5f

	k8s.io/utils => k8s.io/utils v0.0.0-20210111153108-fddb29f9d009
	sigs.k8s.io/cluster-api => sigs.k8s.io/cluster-api v0.4.2
	sigs.k8s.io/controller-tools => sigs.k8s.io/controller-tools v0.5.0
)
