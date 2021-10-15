package status

import (
	"context"
	"sync"
	"time"

	"github.com/quentin-m/etcd-cloud-operator/pkg/etcd"
	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/asg"
	"go.uber.org/zap"
)

type EtcdStatus struct {
	IsHealthy bool
}

type EtcdCache struct {
	value EtcdStatus
	mu    sync.Mutex

	asgCollector *AsgCache
	cfg          *etcd.EtcdConfiguration
	retries      int
	timeout      time.Duration
}

func NewEtcdCache(asgCache *AsgCache, cfg *etcd.EtcdConfiguration, retries int, timeout time.Duration) *EtcdCache {
	return &EtcdCache{
		asgCollector: asgCache,
		cfg:          cfg,
		retries:      retries,
		timeout:      timeout,
	}
}

func (c *EtcdCache) Run(ctx context.Context) {
	for {
		currAsg := c.asgCollector.Value()
		if currAsg.IsValid() {
			client, err := etcd.NewClient(instancesAddresses(currAsg.Instances), c.cfg.ClientTransportSecurity, true)
			if err != nil {
				zap.S().With(zap.Error(err)).With("asg", currAsg).Warn("failed to create etcd cluster client")
			} else {
				start := time.Now()

				etcdHealthy := client.IsHealthy(c.retries, c.timeout)

				if duration := time.Since(start).Round(time.Second); etcdHealthy {
					zap.S().With("asg", currAsg).Debugf("etcd health check completed in %s", duration)
				} else {
					zap.S().With("asg", currAsg).Errorf("etcd health check returned unhealthy in %s", duration)
				}

				c.mu.Lock()
				c.value = EtcdStatus{IsHealthy: etcdHealthy}
				c.mu.Unlock()

				if err := client.Close(); err != nil {
					zap.S().With(zap.Error(err)).Warn("failed to close etcd client")
				}
			}
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func (c *EtcdCache) Value() EtcdStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func instancesAddresses(instances []asg.Instance) (addresses []string) {
	for _, instance := range instances {
		addresses = append(addresses, instance.Address())
	}
	return
}
