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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/quentin-m/etcd-cloud-operator/pkg/etcd"
	"github.com/quentin-m/etcd-cloud-operator/pkg/operator/status"
	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/asg"
	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/snapshot"
	"go.uber.org/zap"
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

	etcdSnapshot *snapshot.Metadata

	state string
}

// Config is the global configuration for an instance of ECO.
type Config struct {
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
	}
}

type worldStatus struct {
	ASG      status.AsgStatus
	Operator status.OperatorStatus
	Etcd     status.EtcdStatus
}

func (w worldStatus) IsValid() bool {
	return w.ASG.IsValid() && w.Operator.IsValid()
}

func (s *Operator) Run(ctx context.Context) error {
	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()

	go func() {
		if err := s.webserver(ctx); err != nil {
			zap.S().With(zap.Error(err)).Warn("status server failed, shutting down")
			shutdown()
		}
	}()

	asgCache := status.NewAsgCache(s.asgProvider)
	operatorCache := status.NewOperatorCache(&http.Client{Timeout: isHealthyTimeout}, webServerPort, asgCache)
	etcdCache := status.NewEtcdCache(asgCache, &s.cfg.Etcd, 3, isHealthyTimeout)

	go asgCache.Run(ctx)
	go operatorCache.Run(ctx)
	go etcdCache.Run(ctx)

	var lastWorld worldStatus
	for {
		asgStatus, operatorStatus, etcdStatus := asgCache.Value(), operatorCache.Value(), etcdCache.Value()
		w := worldStatus{
			ASG:      asgStatus,
			Operator: operatorStatus,
			Etcd:     etcdStatus,
		}
		if w.IsValid() {
			zap.S().With("old", lastWorld, "new", w).Infof("executing state machine")
			start := time.Now()
			if err := s.execute(w); err != nil {
				zap.S().With(zap.Error(err)).Warn("failed to execute state machine")
			}
			zap.S().Infof("state machine executed in %s", time.Since(start).Round(time.Millisecond))
		}
		lastWorld = w

		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			zap.S().Info("STATUS: Shutdown signal received -> Snapshot + Stop")
			s.state = "PENDING"
			if s.server != nil {
				s.server.Stop(lastWorld.Etcd.IsHealthy, true)
			}
			return nil
		}
	}
}

func (s *Operator) execute(w worldStatus) error {
	if s.server == nil {
		s.server = etcd.NewServer(serverConfig(s.cfg, w.ASG.Self, s.snapshotProvider))
	}

	switch {
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case w.Etcd.IsHealthy && !s.server.IsRunning():
		zap.S().Info("STATUS: Healthy + Not running -> Join")
		s.state = "PENDING"

		etcdClient, err := etcd.NewClient(instancesAddresses(w.ASG.Instances), s.cfg.Etcd.ClientTransportSecurity, true)
		if err != nil {
			return fmt.Errorf("failed to create etcd cluster client: %w", err)
		}
		defer func() {
			if err := etcdClient.Close(); err != nil {
				zap.S().With(zap.Error(err)).Error("failed to close etcd client")
			}
		}()
		if err := s.server.Join(etcdClient); err != nil {
			zap.S().With(zap.Error(err)).Error("failed to join the cluster")
		}
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case w.Etcd.IsHealthy && s.server.IsRunning():
		zap.S().Info("STATUS: Healthy + Running -> Standby")
		s.state = "OK"
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case !w.Etcd.IsHealthy && s.server.IsRunning() && time.Since(s.server.Started()) > 30*time.Second && w.Operator.States["OK"] >= w.ASG.ClusterSize/2+1:
		zap.S().Info("STATUS: Unhealthy + Running -> Pending confirmation from other ECO instances")
		s.state = "PENDING"
	case !w.Etcd.IsHealthy && s.server.IsRunning() && time.Since(s.server.Started()) > 30*time.Second && w.Operator.States["OK"] < w.ASG.ClusterSize/2+1:
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
			// TODO: return a non-recoverable error and exit elsewhere
			zap.S().Error("etcd server didn't shut down without graceful timeout, exiting")
			os.Exit(1)
		}
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case !w.Etcd.IsHealthy && !s.server.IsRunning() && (w.Operator.States["START"] != w.ASG.ClusterSize || !w.Operator.IsSeeder):
		if s.state != "START" {
			var err error
			if s.etcdSnapshot, err = s.server.SnapshotInfo(); err != nil && err != snapshot.ErrNoSnapshot {
				return err
			}
		}
		zap.S().Info("STATUS: Unhealthy + Not running -> Ready to start + Pending all ready / seeder")
		s.state = "START"
	////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	case !w.Etcd.IsHealthy && !s.server.IsRunning() && w.Operator.States["START"] == w.ASG.ClusterSize && w.Operator.IsSeeder:
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

	if s.state == "OK" && w.Operator.IsSeeder && s.cfg.Etcd.InitACL != nil {
		etcdClient, err := etcd.NewClient(instancesAddresses(w.ASG.Instances), s.cfg.Etcd.ClientTransportSecurity, true)
		if err != nil {
			return fmt.Errorf("failed to create etcd cluster client: %w", err)
		}
		defer func() {
			if err := etcdClient.Close(); err != nil {
				zap.S().With(zap.Error(err)).Error("failed to close etcd client")
			}
		}()
		if err := s.reconcileInitACLConfig(s.cfg.Etcd.InitACL, etcdClient); err != nil {
			zap.S().With(zap.Error(err)).Error("failed to reconcile initial ACL config")
			return err
		}
	}

	return nil
}

type state struct {
	State    string `json:"state"`
	Revision int64  `json:"revision"`
}

func (s *Operator) webserver(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		st := state{State: s.state}
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
	server := http.Server{
		Addr:         fmt.Sprintf(":%d", webServerPort),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		timeout, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(timeout); err != nil {
			zap.S().With(zap.Error(err)).Info("error shutting down status server")
		} else {
			zap.S().Info("status server shutdown cleanly")
		}
	}()
	zap.S().With("addr", server.Addr).Info("starting status server")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("status server exited with an error: %w", err)
	} else {
		zap.S().Info("status server exited")
	}
	return nil
}
