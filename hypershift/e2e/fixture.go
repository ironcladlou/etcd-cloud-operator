package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openshift/etcd-cloud-operator/e2e/stress"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/quentin-m/etcd-cloud-operator/pkg/etcd"
	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/snapshot"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	InitialClusterReplicasReadySeconds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "etcd_e2e_initial_cluster_replicas_ready_seconds",
		Help: "The time from initial cluster creation until all its pods are observed to be ready.",
	}, []string{"test_id", "test_name"})

	InitialClusterStartupSeconds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "etcd_e2e_initial_cluster_startup_seconds",
		Help: "The total time from initial cluster creation until the etcd cluster reports healthy.",
	}, []string{"test_id", "test_name"})
)

type testCluster struct {
	namespace string
	size      int

	client        *etcd.Client
	clientService string
}

type TestClusterOptions struct {
	LogLevel         string
	ProfilingEnabled bool
	SnapshotsEnabled bool
}

func (o TestClusterOptions) CreateCluster(ctx context.Context, t *testing.T, kubeClient crclient.Client) (*testCluster, func(context.Context), error) {
	start := time.Now()
	testID := ctx.Value("testID").(string)
	testName := strings.ReplaceAll(t.Name(), "/", "_")

	namespace := GenerateName("etcd-")
	clusterSize := 3

	var snapshotConfig snapshot.Config
	if o.SnapshotsEnabled {
		snapshotConfig = snapshot.Config{
			Provider: "file",
			Interval: 5 * time.Minute,
			TTL:      10 * time.Minute,
		}
	} else {
		snapshotConfig = snapshot.Config{
			Provider: "noop",
			Interval: 5 * time.Minute,
		}
	}

	t.Logf("creating etcd cluster in namespace %s", namespace)
	manifests := &Manifests{
		TestID:          testID,
		TestName:        testName,
		TestNamespace:   namespace,
		Replicas:        clusterSize,
		LogLevel:        o.LogLevel,
		EnableProfiling: o.ProfilingEnabled,
		SnapshotConfig:  snapshotConfig,
	}
	resources := []crclient.Object{
		manifests.Namespace(), manifests.ECOConfigMap(), manifests.DiscoveryService(),
		manifests.ClientService(), manifests.StatefulSet(), manifests.ServiceMonitor(),
	}
	for _, obj := range resources {
		t.Logf("creating resource %s/%s/%s", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName())
		if err := kubeClient.Create(ctx, obj); err != nil {
			return nil, nil, fmt.Errorf("failed to create etcd cluster resource: %w", err)
		}
	}

	// Asynchronously record the time it takes the stateful set to report ready
	go func() {
		ss := manifests.StatefulSet().DeepCopy()
		if err := wait.PollUntil(5*time.Second, func() (bool, error) {
			if err := kubeClient.Get(ctx, crclient.ObjectKeyFromObject(ss), ss); err != nil {
				return false, nil
			}
			return *ss.Spec.Replicas == ss.Status.ReadyReplicas, nil
		}, ctx.Done()); err != nil {
			t.Logf("failed to assess status of statefulset: %v", err)
		}
		// TODO: Compare with actual pod start time or add new metric
		InitialClusterReplicasReadySeconds.
			With(prometheus.Labels{"test_id": testID, "test_name": testName}).
			Add(time.Since(start).Seconds())
	}()

	clientService := fmt.Sprintf("client.%s.svc.cluster.local", namespace)
	etcdClient, err := etcd.NewClient([]string{clientService}, etcd.SecurityConfig{}, true)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create etcd cluster client: %w", err)
	}

	t.Logf("verifying cluster health at %s", clientService)
	if err := func() error {
		timeout, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return wait.PollUntil(5*time.Second, func() (bool, error) {
			if _, err := IsHealthy(ctx, etcdClient, clusterSize); err != nil {
				t.Logf("cluster still isn't healthy: %v", err)
				return false, nil
			} else {
				return true, nil
			}
		}, timeout.Done())
	}(); err != nil {
		return nil, nil, fmt.Errorf("failed waiting for cluster to be healthy: %w", err)
	}
	InitialClusterStartupSeconds.
		With(prometheus.Labels{"test_id": testID, "test_name": testName}).
		Add(time.Since(start).Seconds())

	cancelFn := func(ctx context.Context) {
		if err := etcdClient.Close(); err != nil {
			t.Logf("failed to close etcd client: %s", err)
		}

		t.Logf("waiting 10s before deleting cluster to allow metrics scraping")
		<-time.After(10 * time.Second)
		t.Logf("deleting etcd cluster namespace %s", namespace)
		if err := kubeClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			if !apierrors.IsNotFound(err) {
				t.Logf("failed to delete namespace %s: %v", namespace, err)
			}
		}
	}

	return &testCluster{
		namespace:     namespace,
		size:          clusterSize,
		client:        etcdClient,
		clientService: clientService,
	}, cancelFn, nil
}

func (c *testCluster) GetMemberNamesOrDie(ctx context.Context, t *testing.T) []string {
	members, err := c.client.Members()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range members {
		names = append(names, m.Name)
	}
	return names
}

func (c *testCluster) MustBeHealthyAfter(ctx context.Context, t *testing.T, duration time.Duration) {
	t.Logf("waiting for cluster to remain healthy after %s", duration)
	select {
	case <-time.After(duration):
	case <-ctx.Done():
		t.Fatal("test was cancelled")
	}
	timeout, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := IsHealthy(timeout, c.client, c.size); err != nil {
		t.Fatalf("cluster wasn't healthy after %s: %v", duration, err)
	}
}

func (c *testCluster) MustBecomeConsistent(ctx context.Context, t *testing.T, timeout time.Duration) {
	t.Logf("waiting %s for cluster to become consistent", timeout)
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := wait.PollUntil(5*time.Second, func() (bool, error) {
		if err := c.client.IsConsistent(timeoutCtx); err != nil {
			t.Logf("cluster still isn't consistent: %v", err)
			return false, nil
		}
		return true, nil
	}, timeoutCtx.Done()); err != nil {
		t.Fatalf("cluster never became consistent: %v", err)
	}
}

func (c *testCluster) StartStress(ctx context.Context, t *testing.T, warmup time.Duration) func() {
	t.Logf("starting stressor")
	stressorClient, err := etcd.NewClient([]string{c.clientService}, etcd.SecurityConfig{}, true)
	if err != nil {
		t.Fatalf("failed to create etcd cluster client: %v", err)
	}
	stressor := stress.NewStressor()
	stressorCtx, cancelStressor := context.WithCancel(ctx)
	stressor.Start(stressorCtx, c.client.Client)

	t.Logf("warming up stressor for %s", warmup)
	select {
	case <-time.After(warmup):
	case <-ctx.Done():
		t.Fatal("test was cancelled")
	}

	return func() {
		t.Logf("stopping stressor")
		cancelStressor()
		if err := stressorClient.Close(); err != nil {
			t.Logf("failed to close stressor client: %s", err)
		}
	}
}
