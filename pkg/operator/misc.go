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
	"time"

	"github.com/quentin-m/etcd-cloud-operator/pkg/etcd"
	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/asg"
	"github.com/quentin-m/etcd-cloud-operator/pkg/providers/snapshot"
	"go.uber.org/zap"
)

const (
	isHealthyTimeout = 5 * time.Second
)

func initProviders(cfg Config) (asg.Provider, snapshot.Provider) {
	if cfg.ASG.Provider == "" {
		zap.S().Fatal("no auto-scaling group provider configuration given")
	}
	asgProvider, ok := asg.AsMap()[cfg.ASG.Provider]
	if !ok {
		zap.S().Fatalf("unknown auto-scaling group provider %q, available providers: %v", cfg.ASG.Provider, asg.AsList())
	}
	if err := asgProvider.Configure(cfg.ASG); err != nil {
		zap.S().With(zap.Error(err)).Fatal("failed to configure auto-scaling group provider")
	}

	if cfg.Snapshot.Provider == "" {
		return asgProvider, nil
	}
	snapshotProvider, ok := snapshot.AsMap()[cfg.Snapshot.Provider]
	if !ok {
		zap.S().Fatalf("unknown snapshot provider %q, available providers: %v", cfg.Snapshot.Provider, snapshot.AsList())
	}
	if err := snapshotProvider.Configure(cfg.Snapshot); err != nil {
		zap.S().With(zap.Error(err)).Fatal("failed to configure snapshot provider")
	}

	return asgProvider, snapshotProvider
}

func serverConfig(cfg Config, asgSelf asg.Instance, snapshotProvider snapshot.Provider) etcd.ServerConfig {
	return etcd.ServerConfig{
		Name:                    asgSelf.Name(),
		DataDir:                 cfg.Etcd.DataDir,
		DataQuota:               cfg.Etcd.BackendQuota,
		AutoCompactionMode:      cfg.Etcd.AutoCompactionMode,
		AutoCompactionRetention: cfg.Etcd.AutoCompactionRetention,
		BindAddress:             asgSelf.BindAddress(),
		PublicAddress:           stringOverride(asgSelf.Address(), cfg.Etcd.AdvertiseAddress),
		PrivateAddress:          asgSelf.Address(),
		ClientSC:                cfg.Etcd.ClientTransportSecurity,
		PeerSC:                  cfg.Etcd.PeerTransportSecurity,
		UnhealthyMemberTTL:      cfg.UnhealthyMemberTTL,
		SnapshotProvider:        snapshotProvider,
		SnapshotInterval:        cfg.Snapshot.Interval,
		SnapshotTTL:             cfg.Snapshot.TTL,
		JWTAuthTokenConfig:      cfg.Etcd.JWTAuthTokenConfig,
	}
}

func instancesAddresses(instances []asg.Instance) (addresses []string) {
	for _, instance := range instances {
		addresses = append(addresses, instance.Address())
	}
	return
}

func stringOverride(s, override string) string {
	if override != "" {
		return override
	}
	return s
}
