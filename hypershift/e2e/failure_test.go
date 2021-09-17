package e2e

import (
	"context"
	"testing"
	"time"
)

func TestKillLeader(t *testing.T) {
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

	defer RecordTestDuration(ctx, t, time.Now())

	kubeClient := GetKubeClientOrDie(t)

	cluster, destroyCluster := DefaultTestClusterOptions.CreateCluster(ctx, t, kubeClient)
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

	cluster, destroyCluster := DefaultTestClusterOptions.CreateCluster(ctx, t, kubeClient)
	defer destroyCluster(context.Background())

	dm := &DataMarker{Client: cluster.client}
	dm.SetMarkerOrDie(ctx, t)

	stopStress := cluster.StartStress(ctx, t, 10*time.Second)

	t.Logf("killing all members")
	members := cluster.GetMemberNamesOrDie(ctx, t)
	SignalPodsOrDie(ctx, t, cluster.namespace, members)

	// TODO: Supplement or replace with metrics assertions?
	cluster.MustBeHealthyAfter(ctx, t, 30*time.Second)

	stopStress()

	cluster.MustBecomeConsistent(ctx, t, 30*time.Second)

	dm.VerifyOrDie(ctx, t, false)
}

func TestTransientMajorityOutage(t *testing.T) {
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

	defer RecordTestDuration(ctx, t, time.Now())

	kubeClient := GetKubeClientOrDie(t)

	cluster, destroyCluster := DefaultTestClusterOptions.CreateCluster(ctx, t, kubeClient)
	defer destroyCluster(context.Background())

	dm := &DataMarker{Client: cluster.client}
	dm.SetMarkerOrDie(ctx, t)

	t.Logf("inducing majority packet loss")
	impactedMembers := GetRandomMembersOrDie(t, cluster.client, 2)
	InducePacketLossOnPodsOrDie(ctx, t, kubeClient, cluster.namespace, impactedMembers)

	t.Logf("restoring majority connectivity")
	RestorePodConnectivityOrDie(ctx, t, kubeClient, cluster.namespace, impactedMembers[0])

	// TODO: Supplement or replace with metrics assertions?
	cluster.MustBeHealthyAfter(ctx, t, 45*time.Second)

	dm.VerifyOrDie(ctx, t, false)
}

func TestSignals(t *testing.T) {
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

	t.Logf("pausing until signaled...")
	<-ctx.Done()
}

func TestProfiling(t *testing.T) {
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

	defer RecordTestDuration(ctx, t, time.Now())

	kubeClient := GetKubeClientOrDie(t)

	opts := TestClusterOptions{
		LogLevel:        "error",
		EnableProfiling: true,
	}
	_, destroyCluster := opts.CreateCluster(ctx, t, kubeClient)
	defer destroyCluster(context.Background())

	t.Logf("pausing until test is cancelled")
	<-ctx.Done()
}
