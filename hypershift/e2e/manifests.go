package e2e

import (
	"strconv"
	"time"

	"github.com/quentin-m/etcd-cloud-operator/pkg/etcd"
	"github.com/quentin-m/etcd-cloud-operator/pkg/operator"
	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/asg"
	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/snapshot"
	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
)

type Manifests struct {
	TestID          string
	TestName        string
	TestNamespace   string
	Replicas        int
	LogLevel        string
	EnableProfiling bool
}

func (m *Manifests) labels() map[string]string {
	return map[string]string{
		"app":       "etcd",
		"test_id":   m.TestID,
		"test_name": m.TestName,
		"job":       "etcd-e2e-member",
	}
}

func (m *Manifests) Namespace() *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Namespace",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: m.TestNamespace,
			Labels: map[string]string{
				"etcd-e2e": "",
				"testID":   m.TestID,
				"testName": m.TestName,
			},
		},
	}
}

func (m *Manifests) ECOConfigMap() *corev1.ConfigMap {
	type config struct {
		ECO operator.Config `json:"eco"`
	}
	ecoConfig := config{
		ECO: operator.Config{
			CheckInterval:      15 * time.Second,
			UnhealthyMemberTTL: 2 * time.Minute,
			Etcd: etcd.EtcdConfiguration{
				DataDir:                 "/var/lib/etcd",
				BackendQuota:            2 * 1024 * 1024 * 1024,
				AutoCompactionMode:      "periodic",
				AutoCompactionRetention: "0",
			},
			ASG: asg.Config{
				Provider: "sts",
			},
			Snapshot: snapshot.Config{
				Provider: "file",
				Interval: 30 * time.Minute,
				TTL:      24 * time.Hour,
			},
		},
	}
	configBytes, err := yaml.Marshal(ecoConfig)
	if err != nil {
		panic(err)
	}
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: m.TestNamespace,
			Name:      "etcd",
		},
		Data: map[string]string{
			"config.yaml": string(configBytes),
		},
	}
}

func (m *Manifests) DiscoveryService() *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: m.TestNamespace,
			Name:      "discovery",
			Labels:    m.labels(),
		},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 m.labels(),
			Ports: []corev1.ServicePort{
				{
					Name:       "peer",
					Protocol:   corev1.ProtocolTCP,
					Port:       2380,
					TargetPort: intstr.Parse("peer"),
				},
				{
					Name:       "etcd-client",
					Protocol:   corev1.ProtocolTCP,
					Port:       2379,
					TargetPort: intstr.Parse("client"),
				},
				{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       2378,
					TargetPort: intstr.Parse("http"),
				},
				{
					Name:       "metrics",
					Protocol:   corev1.ProtocolTCP,
					Port:       2381,
					TargetPort: intstr.Parse("metrics"),
				},
			},
		},
	}
}

func (m *Manifests) ClientService() *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: corev1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: m.TestNamespace,
			Name:      "client",
			Labels:    m.labels(),
		},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: corev1.ClusterIPNone,
			Selector:  m.labels(),
			Ports: []corev1.ServicePort{
				{
					Name:       "etcd-client",
					Protocol:   corev1.ProtocolTCP,
					Port:       2379,
					TargetPort: intstr.Parse("client"),
				},
			},
		},
	}
}

func (m *Manifests) ServiceMonitor() *unstructured.Unstructured {
	template := `
{
   "apiVersion": "monitoring.coreos.com/v1",
   "kind": "ServiceMonitor",
   "metadata": {
      "name": "discovery"
   },
   "spec": {
      "jobLabel": "job",
      "endpoints": [
         {
            "interval": "5s",
            "port": "metrics"
         }
      ],
			"podTargetLabels": ["test_id", "test_name"]
   }
}
`
	obj, err := runtime.Decode(unstructured.UnstructuredJSONScheme, []byte(template))
	if err != nil {
		panic(err)
	}
	sm := obj.(*unstructured.Unstructured)
	sm.SetNamespace(m.TestNamespace)

	uselector := map[string]interface{}{}
	for k, v := range m.labels() {
		uselector[k] = v
	}
	if err := unstructured.SetNestedMap(sm.Object, uselector, "spec", "selector", "matchLabels"); err != nil {
		panic(err)
	}
	return sm
}

func (m *Manifests) StatefulSet() *appsv1.StatefulSet {
	logLevel := m.LogLevel
	if len(logLevel) == 0 {
		logLevel = "info"
	}
	pprofAddr := ""
	if m.EnableProfiling {
		pprofAddr = ":9999"
	}
	etcdContainer := func(image string, replicas int) *corev1.Container {
		c := &corev1.Container{
			Name: "etcd",
		}
		c.Image = image
		c.ImagePullPolicy = corev1.PullAlways
		c.Command = []string{"/usr/bin/etcd-cloud-operator", "--log-level", logLevel, "--config", "/etc/etcd/config/config.yaml", "--pprof-addr", pprofAddr}
		c.VolumeMounts = []corev1.VolumeMount{
			{
				Name:      "data",
				MountPath: "/var/lib",
			},
			{
				Name:      "config",
				MountPath: "/etc/etcd/config",
			},
		}
		c.Env = []corev1.EnvVar{
			{
				Name:  "ETCD_API",
				Value: "3",
			},
			{
				Name:  "ETCDCTL_INSECURE_SKIP_TLS_VERIFY",
				Value: "true",
			},
			{
				Name:  "STATEFULSET_SERVICE_NAME",
				Value: "discovery",
			},
			{
				Name:  "STATEFULSET_NAME",
				Value: "etcd",
			},
			{
				Name:  "STATEFULSET_DNS_CLUSTER_SUFFIX",
				Value: "cluster.local",
			},
			{
				Name: "POD_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "status.podIP",
					},
				},
			},
			{
				Name: "STATEFULSET_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					},
				},
			},
			// TODO: Find a way to avoid encoding this in env
			{
				Name:  "STATEFULSET_REPLICAS",
				Value: strconv.Itoa(replicas),
			},
		}
		c.Ports = []corev1.ContainerPort{
			{
				Name:          "client",
				ContainerPort: 2379,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "http",
				ContainerPort: 2378,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "peer",
				ContainerPort: 2380,
				Protocol:      corev1.ProtocolTCP,
			},
			{
				Name:          "metrics",
				ContainerPort: 2381,
				Protocol:      corev1.ProtocolTCP,
			},
		}
		c.LivenessProbe = &corev1.Probe{
			Handler: corev1.Handler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/status",
					Port: intstr.Parse("http"),
				},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		}
		c.ReadinessProbe = &corev1.Probe{
			Handler: corev1.Handler{
				TCPSocket: &corev1.TCPSocketAction{
					Port: intstr.Parse("client"),
				},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       1,
			FailureThreshold:    10,
		}
		c.StartupProbe = &corev1.Probe{
			Handler: corev1.Handler{
				Exec: &corev1.ExecAction{
					Command: []string{"/bin/sh", "-c", "/usr/bin/etcdctl --endpoints=${HOSTNAME}:2379 endpoint health"},
				},
			},
			FailureThreshold: 60,
			PeriodSeconds:    1,
		}
		return c
	}

	return &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: appsv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "etcd",
			Namespace: m.TestNamespace,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: m.DiscoveryService().Name,
			Selector: &metav1.LabelSelector{
				MatchLabels: m.labels(),
			},
			Replicas:            pointer.Int32Ptr(int32(m.Replicas)),
			PodManagementPolicy: appsv1.ParallelPodManagement,
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						StorageClassName: pointer.StringPtr("gp2"),
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: m.labels(),
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						*etcdContainer("quay.io/dmace/etcd-cloud-operator:latest", 3),
					},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: m.ECOConfigMap().Name,
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
