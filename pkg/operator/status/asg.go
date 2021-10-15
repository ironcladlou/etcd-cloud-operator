package status

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/asg"
	"go.uber.org/zap"
)

type AsgStatus struct {
	Instances   asg.Instances
	Self        asg.Instance
	ClusterSize int
}

func (a *AsgStatus) IsValid() bool {
	return len(a.Instances) > 0
}

type AsgCache struct {
	value AsgStatus
	mu    sync.Mutex

	provider asg.Provider
}

func NewAsgCache(provider asg.Provider) *AsgCache {
	return &AsgCache{
		provider: provider,
	}
}

func (c *AsgCache) Run(ctx context.Context) {
	for {
		start := time.Now()
		asgInstances, asgSelf, asgSize, err := c.provider.AutoScalingGroupStatus()
		zap.S().With("instances", asg.Instances(asgInstances), "self", asgSelf, "size", asgSize).Debugf("asg status evaluation completed in %s", time.Since(start).Round(time.Millisecond))
		if err != nil {
			zap.S().With(zap.Error(err)).Warn("failed to sync auto-scaling group")
		} else {
			sort.Sort(asg.ByName(asgInstances))
			c.mu.Lock()
			c.value = AsgStatus{
				Instances:   asgInstances,
				Self:        asgSelf,
				ClusterSize: asgSize,
			}
			c.mu.Unlock()
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func (c *AsgCache) Value() AsgStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}
