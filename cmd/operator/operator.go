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

// Package main implements basic logic to start the etcd-cloud-operator.
package main

import (
	"context"
	"flag"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/quentin-m/etcd-cloud-operator/pkg/logger"
	"github.com/quentin-m/etcd-cloud-operator/pkg/operator"

	// Register providers.
	_ "github.com/quentin-m/etcd-cloud-operator/pkg/providers/asg/aws"
	_ "github.com/quentin-m/etcd-cloud-operator/pkg/providers/asg/docker"
	_ "github.com/quentin-m/etcd-cloud-operator/pkg/providers/asg/sts"
	_ "github.com/quentin-m/etcd-cloud-operator/pkg/providers/snapshot/file"
	_ "github.com/quentin-m/etcd-cloud-operator/pkg/providers/snapshot/s3"
)

func main() {
	// Parse command-line arguments.
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	flagConfigPath := flag.String("config", "", "Load configuration from the specified file.")
	flagLogLevel := flag.String("log-level", "info", "Define the logging level.")
	flagPprofAddr := flag.String("pprof-addr", "", "Serve pprof metrics on addr")
	flag.Parse()

	// Initialize logging system.
	logger.Configure(*flagLogLevel)

	// Read configuration.
	config, err := loadConfig(*flagConfigPath)
	if err != nil {
		zap.S().With(zap.Error(err)).Fatal("failed to load configuration")
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()

	if addr := *flagPprofAddr; len(addr) > 0 {
		go startPprofServer(ctx, addr)
	}

	// Run.
	if err := operator.New(config.ECO).Run(ctx); err != nil {
		zap.S().With(zap.Error(err)).Fatal("operator failed with an error")
	} else {
		zap.S().Info("operator is exiting gracefully")
	}
}

func startPprofServer(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	server := http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		timeout, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(timeout); err != nil {
			zap.S().With(zap.Error(err)).Info("error shutting down pprof server")
		} else {
			zap.S().Info("pprof server shutdown", "addr", addr)
		}
	}()
	zap.S().Info("starting pprof server", "addr", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		zap.S().With(zap.Error(err)).Fatalf("error starting pprof server")
	} else {
		zap.S().Info("pprof server returned", "addr", addr)
	}
}
