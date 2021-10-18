package e2e

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

func TestInteractiveCluster(t *testing.T) {
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

	defer RecordTestDuration(ctx, t, time.Now())

	kubeClient := GetKubeClientOrDie(t)

	clusterOpts := opts.DefaultClusterOptions()
	clusterOpts.Size = 3

	_, destroyCluster, err := clusterOpts.CreateCluster(ctx, t, kubeClient)
	if err != nil {
		t.Fatalf("failed to create cluster: %v", err)
	}
	defer destroyCluster(context.Background())

	t.Logf("pausing until test is cancelled")
	<-ctx.Done()
}

func TestKillLeader(t *testing.T) {
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

	defer RecordTestDuration(ctx, t, time.Now())

	kubeClient := GetKubeClientOrDie(t)

	clusterOpts := opts.DefaultClusterOptions()
	clusterOpts.Size = 3

	cluster, destroyCluster, err := clusterOpts.CreateCluster(ctx, t, kubeClient)
	if err != nil {
		t.Fatalf("failed to create cluster: %v", err)
	}
	defer destroyCluster(context.Background())

	dm := &DataMarker{Client: cluster.client}
	dm.SetMarkerOrDie(ctx, t)

	stopStress := cluster.StartStress(ctx, t, 10*time.Second)

	// TODO: This is still racy, if the race can't be solved we at least need
	// to detect it after the fact, maybe by flagging if no raft term changes
	// following the leader kill?
	// TODO: Retry leader disagreement errors
	t.Logf("killing leader")
	leader, _ := GetLeaderOrDie(ctx, t, cluster.client)
	SignalPodOrDie(ctx, t, cluster.namespace, leader, "ABRT")

	// TODO: Supplement or replace with metrics assertions?
	cluster.MustBeHealthyAfter(ctx, t, 30*time.Second)

	stopStress()

	cluster.MustBecomeConsistent(ctx, t, 30*time.Second)

	dm.VerifyOrDie(ctx, t, false)
}

func TestKillAll(t *testing.T) {
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

	defer RecordTestDuration(ctx, t, time.Now())

	kubeClient := GetKubeClientOrDie(t)

	clusterOpts := opts.DefaultClusterOptions()
	clusterOpts.Size = 3

	cluster, destroyCluster, err := clusterOpts.CreateCluster(ctx, t, kubeClient)
	if err != nil {
		t.Fatalf("failed to create cluster: %v", err)
	}
	defer destroyCluster(context.Background())

	dm := &DataMarker{Client: cluster.client}
	dm.SetMarkerOrDie(ctx, t)

	stopStress := cluster.StartStress(ctx, t, 10*time.Second)

	t.Logf("killing all members")
	members := cluster.GetMemberNamesOrDie(ctx, t)
	SignalPodsOrDie(ctx, t, cluster.namespace, members)

	if err := wait.PollUntil(5*time.Second, func() (bool, error) {
		if _, err := IsHealthy(ctx, cluster.client, cluster.size); err != nil {
			t.Logf("cluster still isn't healthy: %v", err)
			return false, nil
		} else {
			return true, nil
		}
	}, ctx.Done()); err != nil {
		t.Fatalf("cluster didn't become healthy before timeout: %s", err)
	}

	stopStress()

	if err := wait.PollUntil(5*time.Second, func() (bool, error) {
		if err := cluster.client.IsConsistent(ctx); err != nil {
			t.Logf("cluster still isn't consistent: %v", err)
			return false, nil
		}
		return true, nil
	}, ctx.Done()); err != nil {
		t.Fatalf("cluster didn't became consistent: %v", err)
	}

	dm.VerifyOrDie(ctx, t, false)
}

// TestTransientMajorityOutage ensures that given a transient network outage
// impacting a majority of members temporarily and 1 of those members permanently,
// that linearized reads eventually succeed. Quorum is not expected to be lost
// in this scenario.
func TestTransientMajorityOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

	defer RecordTestDuration(ctx, t, time.Now())

	kubeClient := GetKubeClientOrDie(t)

	clusterOpts := opts.DefaultClusterOptions()
	clusterOpts.Size = 3

	cluster, destroyCluster, err := clusterOpts.CreateCluster(ctx, t, kubeClient)
	if err != nil {
		t.Fatalf("failed to create cluster: %v", err)
	}
	defer destroyCluster(context.Background())

	dm := &DataMarker{Client: cluster.client}
	dm.SetMarkerOrDie(ctx, t)

	t.Logf("inducing majority packet loss")
	impactedMembers := GetRandomMembersOrDie(t, cluster.client, 2)
	InducePacketLossOnPodsOrDie(ctx, t, kubeClient, cluster.namespace, impactedMembers)

	t.Logf("restoring majority connectivity")
	RestorePodConnectivityOrDie(ctx, t, kubeClient, cluster.namespace, impactedMembers[0])

	if err := wait.PollUntil(5*time.Second, func() (bool, error) {
		if !cluster.client.IsHealthy(1, 5*time.Second) {
			t.Logf("waiting for cluster to become healthy")
			return false, nil
		}
		return true, nil
	}, ctx.Done()); err != nil {
		t.Fatalf("cluster didn't become healthy before timeout: %s", err)
	}

	dm.VerifyOrDie(ctx, t, false)
}
