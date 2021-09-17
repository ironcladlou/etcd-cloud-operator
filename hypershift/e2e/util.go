// Copyright 2017 Quentin Machu & eco authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e

import (
	"context"
	"fmt"
	"math/rand"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quentin-m/etcd-cloud-operator/pkg/etcd"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/errors"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	cr "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type DataMarker struct {
	Client *etcd.Client
	time   time.Time
}

func (dm *DataMarker) SetMarkerOrDie(ctx context.Context, t *testing.T) {
	if err := dm.SetMarker(ctx); err != nil {
		t.Fatalf("failed to set marker: %s", err)
	}
}

func (dm *DataMarker) SetMarker(ctx context.Context) error {
	dm.time = time.Now()
	data, _ := dm.time.MarshalText()

	_, err := dm.Client.Put(ctx, "/marker", string(data))
	return err
}

func (dm *DataMarker) VerifyOrDie(ctx context.Context, t *testing.T, isLossy bool) {
	if err := dm.Verify(ctx, isLossy); err != nil {
		t.Fatalf("failed to verify marker: %s", err)
	}
}

func (dm *DataMarker) Verify(ctx context.Context, isLossy bool) error {
	var t time.Time

	resp, err := dm.Client.Get(ctx, "/marker")
	if err != nil || len(resp.Kvs) == 0 {
		return fmt.Errorf("failed to verify data marker: it has disappeared - cluster might have reset")
	}
	if err := t.UnmarshalText(resp.Kvs[0].Value); err != nil {
		return fmt.Errorf("failed to verify data marker: unexpected read value: %w", err)
	}

	if !t.Equal(dm.time) {
		zap.S().Infof("data marker was reset the value it held %v ago", time.Since(t))

		if !isLossy {
			return fmt.Errorf("failed to verify data marker: unexpected value - cluster might have been restored to an old snapshot unexpectedly")
		}
	}
	return nil
}

func GenerateName(base string) string {
	maxNameLength := 63
	randomLength := 5
	maxGeneratedNameLength := maxNameLength - randomLength
	if len(base) > maxGeneratedNameLength {
		base = base[:maxGeneratedNameLength]
	}
	return fmt.Sprintf("%s%s", base, utilrand.String(randomLength))
}

func GetKubeClientOrDie(t *testing.T) crclient.Client {
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	appsv1.AddToScheme(scheme)
	rbacv1.AddToScheme(scheme)

	cfg, err := cr.GetConfig()
	if err != nil {
		t.Fatalf("couldn't get client config: %v", err)
	}
	cfg.QPS = 100
	cfg.Burst = 100
	client, err := crclient.New(cfg, crclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("couldn't build client: %v", err)
	}
	return client
}

func IsHealthy(parent context.Context, c *etcd.Client, clusterSize int) (uint64, error) {
	err := func() error {
		timeout, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		_, err := c.Get(timeout, "health")
		if err == nil || err == rpctypes.ErrPermissionDenied || err == rpctypes.ErrGRPCCompacted {
			return nil
		}
		return fmt.Errorf("cluster health endpoint returned an error: %w", err)
	}()
	if err != nil {
		return 0, err
	}

	var healthyMembers, unhealthyMembers []string
	var leader uint64
	var leaderChanged bool

	c.ForEachMember(func(c *etcd.Client, m *etcdserverpb.Member) error {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()

		cURL := etcd.URL2Address(m.PeerURLs[0])

		resp, err := c.Status(ctx, etcd.ClientURL(cURL, c.SC.TLSEnabled()))
		if err != nil {
			unhealthyMembers = append(unhealthyMembers, cURL)
			return nil
		}
		healthyMembers = append(healthyMembers, cURL)

		if leader == 0 {
			leader = resp.Leader
		} else if resp.Leader != leader {
			leaderChanged = true
		}

		return nil
	})

	if leaderChanged {
		return leader, fmt.Errorf("leader has changed during member listing, or all members do not agree on one")
	}
	if len(unhealthyMembers) > 0 {
		return leader, fmt.Errorf("cluster has unhealthy members: %v", unhealthyMembers)
	}
	if len(healthyMembers) != clusterSize {
		return leader, fmt.Errorf("cluster has %d healthy members, expected %d", len(healthyMembers), clusterSize)
	}

	// Verify no alarms are firing.
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	alarms, err := c.AlarmList(ctx)
	if err != nil {
		return leader, fmt.Errorf("failed to retrieve alarms: %s", err)
	}
	if len(alarms.Alarms) > 0 {
		return leader, fmt.Errorf("cluster has alarms firing")
	}

	return leader, nil
}

func GetLeaderOrDie(ctx context.Context, t *testing.T, c *etcd.Client) (string, uint64) {
	name, id, err := GetLeader(ctx, c)
	if err != nil {
		t.Fatalf("failed to get leader: %s", err)
	}
	return name, id
}

func GetLeader(ctx context.Context, c *etcd.Client) (string, uint64, error) {
	var mu sync.Mutex
	var name string
	var leader uint64
	var leaderChanged bool
	err := c.ForEachMember(func(c *etcd.Client, m *etcdserverpb.Member) error {
		timeout, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		cURL := etcd.URL2Address(m.PeerURLs[0])

		resp, err := c.Status(timeout, etcd.ClientURL(cURL, c.SC.TLSEnabled()))
		if err != nil {
			return nil
		}

		mu.Lock()
		defer mu.Unlock()
		if leader == 0 {
			leader = resp.Leader
			name = m.Name
		} else if resp.Leader != leader {
			leaderChanged = true
		}

		return nil
	})
	if err != nil {
		return "", 0, err
	}
	if leaderChanged {
		return "", leader, fmt.Errorf("leader has changed during member listing, or all members do not agree on one")
	}
	return name, leader, nil
}

func GetRandomMembersOrDie(t *testing.T, c *etcd.Client, count int) []string {
	members, err := RandomMembers(c, count)
	if err != nil {
		t.Fatalf("failed to get random members: %s", err)
	}
	return members
}

func RandomMembers(c *etcd.Client, count int) ([]string, error) {
	var names []string
	members, err := c.Members()
	if err != nil {
		return names, err
	}
	if count < 2 || count > len(members) {
		return names, fmt.Errorf("count (%d) must be between 2 and the number of members (%d)", count, len(members))
	}
	indexes := rand.Perm(count)
	for _, i := range indexes {
		names = append(names, members[i].Name)
	}
	return names, nil
}

func ForEachPod(ctx context.Context, namespace string, pods []string, fn func(ctx context.Context, name string) error) error {
	wg := sync.WaitGroup{}
	wg.Add(len(pods))
	var errs []error
	mu := sync.Mutex{}
	for _, pod := range pods {
		go func(name string) {
			defer wg.Done()
			err := fn(ctx, name)
			if err != nil {
				mu.Lock()
				defer mu.Unlock()
				errs = append(errs, err)
			}
		}(pod)
	}
	wg.Wait()
	return errors.NewAggregate(errs)
}

func SignalPodOrDie(ctx context.Context, t *testing.T, namespace, name, sig string) {
	if err := SignalPod(ctx, namespace, name, sig); err != nil {
		t.Fatalf("failed to signal pod: %s", err)
	}
}

func SignalPod(ctx context.Context, namespace string, name string, sig string) error {
	cmd := "/usr/bin/oc"
	args := []string{"exec", "--namespace", namespace, name, "--", "/bin/sh", "-c", fmt.Sprintf("kill -%s 1", sig)}
	out, err := exec.CommandContext(ctx, cmd, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to signal member %s: %w (output: %s)", name, err, out)
	}
	return nil
}

func SignalPodsOrDie(ctx context.Context, t *testing.T, namespace string, pods []string) {
	if err := SignalPods(ctx, namespace, pods); err != nil {
		t.Fatalf("failed to signal pods: %s", err)
	}
}

func SignalPods(ctx context.Context, namespace string, pods []string) error {
	return ForEachPod(ctx, namespace, pods, func(ctx context.Context, name string) error {
		return SignalPod(ctx, namespace, name, "ABRT")
	})
}

func InducePacketLossOnPodsOrDie(ctx context.Context, t *testing.T, kubeClient client.Client, namespace string, pods []string) {
	if err := InducePacketLossOnPods(ctx, kubeClient, namespace, pods); err != nil {
		t.Fatalf("failed to induce pod packet loss: %s", err)
	}
}

func InducePacketLossOnPods(ctx context.Context, kubeClient client.Client, namespace string, pods []string) error {
	return ForEachPod(ctx, namespace, pods, func(ctx context.Context, name string) error {
		return InducePacketLossOnPod(ctx, kubeClient, namespace, name)
	})
}

func InducePacketLossOnPod(ctx context.Context, kubeClient client.Client, namespace string, name string) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}
	if err := kubeClient.Get(ctx, crclient.ObjectKeyFromObject(pod), pod); err != nil {
		return fmt.Errorf("failed to get pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	containerPID, err := GetEtcdContainerPID(ctx, pod)
	if err != nil {
		return err
	}

	out, err := exec.Command("/usr/bin/oc", "debug", "--quiet", "nodes/"+pod.Spec.NodeName, "--", "chroot", "/host", "nsenter", "-t", containerPID, "-n", "tc", "qdisc", "add", "dev", "eth0", "root", "netem", "loss", "100%").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to execute tc against pod %s/%s with PID %s: %w, output:%s", pod.Namespace, pod.Name, containerPID, err, out)
	}
	return nil
}

func RestorePodConnectivityOrDie(ctx context.Context, t *testing.T, kubeClient client.Client, namespace, name string) {
	if err := RestorePodConnectivity(ctx, kubeClient, namespace, name); err != nil {
		t.Fatalf("failed to restore pod connectivity: %s", err)
	}
}

func RestorePodConnectivity(ctx context.Context, kubeClient client.Client, namespace string, name string) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}
	if err := kubeClient.Get(ctx, crclient.ObjectKeyFromObject(pod), pod); err != nil {
		return fmt.Errorf("failed to get pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	containerPID, err := GetEtcdContainerPID(ctx, pod)
	if err != nil {
		return err
	}

	out, err := exec.Command("/usr/bin/oc", "debug", "--quiet", "nodes/"+pod.Spec.NodeName, "--", "chroot", "/host", "nsenter", "-t", containerPID, "-n", "tc", "qdisc", "del", "dev", "eth0", "root", "netem", "loss", "100%").CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to execute tc against pod %s/%s with PID %s: %w, output: %s", pod.Namespace, pod.Name, containerPID, err, out)
	}
	return nil
}

func GetEtcdContainerPID(ctx context.Context, pod *corev1.Pod) (string, error) {
	containerID := pod.Status.ContainerStatuses[0].ContainerID[8:]

	oc := "/usr/bin/oc"
	args := []string{"debug", "--quiet", "nodes/" + pod.Spec.NodeName, "--", "chroot", "/host", "crictl", "inspect", "-o", "go-template", "--template", "{{.info.pid}}", containerID}
	out, err := exec.CommandContext(ctx, oc, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("failed to get container PID for pod %s/%s with container ID %s: %w, output: %s", pod.Namespace, pod.Name, containerID, err, out)
	}
	return strings.ReplaceAll(string(out), "\n", ""), nil
}
