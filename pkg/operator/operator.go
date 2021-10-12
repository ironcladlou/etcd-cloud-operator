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

package operator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/quentin-m/etcd-cloud-operator/pkg/etcd"
	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/asg"
	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/snapshot"
)

const (
	webServerPort = 2378
)

type Operator struct {
	server *etcd.Server

	// New()
	cfg              Config
	asgProvider      asg.Provider
	snapshotProvider snapshot.Provider

	shutdownChan chan os.Signal
	shutdown     bool

	etcdSnapshot *snapshot.Metadata

	state string
}

// Config is the global configuration for an instance of ECO.
type Config struct {
	CheckInterval      time.Duration `yaml:"check-interval"`
	UnhealthyMemberTTL time.Duration `yaml:"unhealthy-member-ttl"`

	Etcd     etcd.EtcdConfiguration `yaml:"etcd"`
	ASG      asg.Config             `yaml:"asg"`
	Snapshot snapshot.Config        `yaml:"snapshot"`
}

func New(cfg Config) *Operator {
	// Initialize providers.
	asgProvider, snapshotProvider := initProviders(cfg)
	if snapshotProvider == nil || cfg.Snapshot.Interval == 0 {
		zap.S().Fatal("snapshots must be enabled for disaster recovery")
	}

	// Setup signal handler.
	shutdownChan := make(chan os.Signal, 1)
	signal.Notify(shutdownChan, syscall.SIGTERM)

	return &Operator{
		cfg:              cfg,
		asgProvider:      asgProvider,
		snapshotProvider: snapshotProvider,
		state:            "UNKNOWN",
		shutdownChan:     shutdownChan,
	}
}

type AsgStatus struct {
	Instances   asg.Instances
	Self        asg.Instance
	ClusterSize int
}

func (a AsgStatus) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddObject("instances", a.Instances)
	enc.AddString("self", a.Self.String())
	enc.AddInt("clusterSize", a.ClusterSize)
	return nil
}

type EcoStatus struct {
	States   map[string]int
	IsSeeder bool
}

type EtcdStatus struct {
	IsHealthy bool
}

type World struct {
	ASG  *AsgStatus
	ECO  *EcoStatus
	Etcd *EtcdStatus
}

func (a World) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if a.ASG != nil {
		enc.AddObject("asg", a.ASG)
	}
	if a.ECO != nil {
		enc.AddReflected("eco", a.ECO)
	}
	if a.Etcd != nil {
		enc.AddReflected("etcd", a.Etcd)
	}
	return nil
}

type asgCollector struct {
	value *AsgStatus
	mu    sync.Mutex
}

func (c *asgCollector) run(provider asg.Provider) {
	for {
		start := time.Now()
		asgInstances, asgSelf, asgSize, err := provider.AutoScalingGroupStatus()
		zap.S().With("instances", asg.Instances(asgInstances), "self", asgSelf, "size", asgSize).Debugf("asg status evaluation completed in %s", time.Since(start).Round(time.Millisecond))
		if err != nil {
			zap.S().With(zap.Error(err)).Warn("failed to sync auto-scaling group")
		} else {
			c.mu.Lock()
			c.value = &AsgStatus{
				Instances:   asgInstances,
				Self:        asgSelf,
				ClusterSize: asgSize,
			}
			c.mu.Unlock()
		}
		time.Sleep(5 * time.Second)
	}
}

func (c *asgCollector) get() *AsgStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

type ecoCollector struct {
	value *EcoStatus
	mu    sync.Mutex
}

func (c *ecoCollector) run(client *http.Client, asgCollector *asgCollector) {
	for {
		currAsg := asgCollector.get()
		if currAsg != nil {
			start := time.Now()
			isSeeder, states := fetchEcoStatuses(client, currAsg.Instances, currAsg.Self)
			zap.S().With("asg", currAsg).Debugf("eco status evaluation completed in %s", time.Since(start).Round(time.Millisecond))
			c.mu.Lock()
			c.value = &EcoStatus{
				States:   states,
				IsSeeder: isSeeder,
			}
			c.mu.Unlock()
		}
		time.Sleep(1 * time.Second)
	}
}

func (c *ecoCollector) get() *EcoStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

type etcdCollector struct {
	value *EtcdStatus
	mu    sync.Mutex
}

func (c *etcdCollector) run(asgCollector *asgCollector, cfg *Config) {
	for {
		currAsg := asgCollector.get()
		if currAsg != nil {
			client, err := etcd.NewClient(instancesAddresses(currAsg.Instances), cfg.Etcd.ClientTransportSecurity, true)
			if err != nil {
				zap.S().With(zap.Error(err)).With("asg", currAsg).Warn("failed to create etcd cluster client")
			} else {
				start := time.Now()
				etcdHealthy := client.IsHealthy(isHealthRetries, isHealthyTimeout)
				zap.S().With("asg", currAsg).Infof("etcd health check completed in %s", time.Since(start).Round(time.Second))

				c.mu.Lock()
				c.value = &EtcdStatus{IsHealthy: etcdHealthy}
				c.mu.Unlock()

				if err := client.Close(); err != nil {
					zap.S().With(zap.Error(err)).Warn("failed to close etcd client")
				}
			}
		}
		time.Sleep(1 * time.Second)
	}
}

func (c *etcdCollector) get() *EtcdStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.value == nil {
		return &EtcdStatus{IsHealthy: false}
	}
	return c.value
}

type worldCollector struct {
	value *World
	mu    sync.Mutex
}

func (c *worldCollector) run(asgC *asgCollector, ecoC *ecoCollector, etcdC *etcdCollector) {
	for {
		asgStatus, ecoStatus, etcdStatus := asgC.get(), ecoC.get(), etcdC.get()
		c.mu.Lock()
		c.value = &World{
			ASG:  asgStatus,
			ECO:  ecoStatus,
			Etcd: etcdStatus,
		}
		c.mu.Unlock()
		time.Sleep(1 * time.Second)
	}
}

func (c *worldCollector) get() *World {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func (s *Operator) Run() {
	go s.webserver()

	asgC := &asgCollector{}
	ecoC := &ecoCollector{}
	etcdC := &etcdCollector{}
	worldC := &worldCollector{}

	go asgC.run(s.asgProvider)
	go ecoC.run(&http.Client{Timeout: isHealthyTimeout}, asgC)
	go etcdC.run(asgC, &s.cfg)
	go worldC.run(asgC, ecoC, etcdC)

	go func() {
		for {
			w := worldC.get()
			if w != nil && w.ECO != nil && w.ASG != nil {
				start := time.Now()
				if err := s.execute(w); err != nil {
					zap.S().With(zap.Error(err)).Warn("failed to execute state machine")
				}
				zap.S().With("world", w).Infof("state machine executed in %s", time.Since(start).Round(time.Millisecond))
			}
			time.Sleep(1 * time.Second)
		}
	}()

	<-s.shutdownChan
}

func (s *Operator) execute(w *World) error {
	if s.server == nil {
		s.server = etcd.NewServer(serverConfig(s.cfg, w.ASG.Self, s.snapshotProvider))
	}

	etcdClient, err := etcd.NewClient(instancesAddresses(w.ASG.Instances), s.cfg.Etcd.ClientTransportSecurity, true)
	if err != nil {
		return fmt.Errorf("failed to create etcd cluster client: %w", err)
	}
	defer etcdClient.Close()

	switch {
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case s.shutdown:
		zap.S().Info("STATUS: Received SIGTERM -> Snapshot + Stop")
		s.state = "PENDING"

		s.server.Stop(w.Etcd.IsHealthy, true)
		os.Exit(0)
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case w.Etcd.IsHealthy && !s.server.IsRunning():
		zap.S().Info("STATUS: Healthy + Not running -> Join")
		s.state = "PENDING"

		if err := s.server.Join(etcdClient); err != nil {
			zap.S().With(zap.Error(err)).Error("failed to join the cluster")
		}
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case w.Etcd.IsHealthy && s.server.IsRunning():
		zap.S().Info("STATUS: Healthy + Running -> Standby")
		s.state = "OK"
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case !w.Etcd.IsHealthy && s.server.IsRunning() && time.Since(s.server.Started()) > 30*time.Second && w.ECO.States["OK"] >= w.ASG.ClusterSize/2+1:
		zap.S().Info("STATUS: Unhealthy + Running -> Pending confirmation from other ECO instances")
		s.state = "PENDING"
	case !w.Etcd.IsHealthy && s.server.IsRunning() && time.Since(s.server.Started()) > 30*time.Second && w.ECO.States["OK"] < w.ASG.ClusterSize/2+1:
		zap.S().Info("STATUS: Unhealthy + Running + No quorum -> Snapshot + Stop")
		s.state = "PENDING"

		// TODO: (dan) the call to EtcdServer.HardStop can block for up to 15m in the
		// partition scenario, so enforce a timeout
		done := make(chan struct{})
		go func() {
			s.server.Stop(false, true)
			done <- struct{}{}
		}()
		select {
		case <-done:
			zap.S().Info("etcd server successfully stopped")
		case <-time.After(15 * time.Second):
			zap.S().Error("etcd server didn't shut down without graceful timeout, exiting")
			os.Exit(0)
		}
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case !w.Etcd.IsHealthy && !s.server.IsRunning() && (w.ECO.States["START"] != w.ASG.ClusterSize || !w.ECO.IsSeeder):
		if s.state != "START" {
			var err error
			if s.etcdSnapshot, err = s.server.SnapshotInfo(); err != nil && err != snapshot.ErrNoSnapshot {
				return err
			}
		}
		zap.S().Info("STATUS: Unhealthy + Not running -> Ready to start + Pending all ready / seeder")
		s.state = "START"
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case !w.Etcd.IsHealthy && !s.server.IsRunning() && w.ECO.States["START"] == w.ASG.ClusterSize && w.ECO.IsSeeder:
		zap.S().Info("STATUS: Unhealthy + Not running + All ready + Seeder status -> Seeding cluster")
		s.state = "START"

		if err := s.server.Seed(s.etcdSnapshot); err != nil {
			zap.S().With(zap.Error(err)).Error("failed to seed the cluster")
		}
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	default:
		s.state = "UNKNOWN"
		return errors.New("no adequate action found")
		////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	}

	if s.state == "OK" && w.ECO.IsSeeder && s.cfg.Etcd.InitACL != nil {
		if err := s.reconcileInitACLConfig(s.cfg.Etcd.InitACL, etcdClient); err != nil {
			zap.S().With(zap.Error(err)).Error("failed to reconcile initial ACL config")
			return err
		}
	}

	return nil
}

func (s *Operator) webserver() {
	http.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		st := status{State: s.state}
		if s.etcdSnapshot != nil {
			st.Revision = s.etcdSnapshot.Revision
		}
		b, err := json.Marshal(&st)
		if err != nil {
			zap.S().With(zap.Error(err)).Warn("failed to marshal status")
			return
		}
		if _, err := w.Write(b); err != nil {
			zap.S().With(zap.Error(err)).Warn("failed to write status")
		}
	})
	zap.S().Fatal(http.ListenAndServe(fmt.Sprintf(":%d", webServerPort), nil))
}
