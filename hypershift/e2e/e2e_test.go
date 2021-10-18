package e2e

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/openshift/etcd-cloud-operator/e2e/stress"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/quentin-m/etcd-cloud-operator/pkg/logger"
	"go.uber.org/zap"
)

var (
	TestDurationSeconds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "etcd_e2e_test_duration_seconds",
		Help: "The total duration of the test.",
	}, []string{"test_id", "test_name"})
)

func RecordTestDuration(ctx context.Context, t *testing.T, start time.Time) {
	testID := ctx.Value("testID").(string)
	testName := strings.ReplaceAll(t.Name(), "/", "_")
	TestDurationSeconds.
		With(prometheus.Labels{"test_id": testID, "test_name": testName}).
		Add(time.Since(start).Seconds())
}

var (
	// opts are global options for the test suite bound in TestMain.
	opts = &GlobalOptions{}

	// globalCtx should be used as the parent context for any test code, and will
	// be cancelled if a SIGINT or SIGTERM is received. It's set up in TestMain.
	globalCtx context.Context
)

// options are global test options applicable to all scenarios.
type GlobalOptions struct {
	LogLevel              string
	MetricsAddr           string
	defaultClusterOptions TestClusterOptions
}

func (o GlobalOptions) DefaultClusterOptions() TestClusterOptions {
	return o.defaultClusterOptions
}

// TestMain deals with global options and setting up a signal-bound context
// for all tests to use.
func TestMain(m *testing.M) {
	flag.StringVar(&opts.LogLevel, "log-level", "info", "logging level")
	flag.StringVar(&opts.MetricsAddr, "metrics-addr", ":8000", "metrics bind address")
	flag.StringVar(&opts.defaultClusterOptions.EtcdOperatorImage, "etcd-operator-image", "quay.io/dmace/etcd-cloud-operator:latest", "The etcd operator image to test")
	flag.StringVar(&opts.defaultClusterOptions.LogLevel, "etcd-log-level", "info", "logging level for etcd test clusters")
	flag.BoolVar(&opts.defaultClusterOptions.ProfilingEnabled, "profiling-enabled", false, "enables profiling on test clusters")
	flag.Parse()

	logger.Configure(opts.LogLevel)

	// Set up a root context for all tests and set up signal handling
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		zap.S().Info("tests received shutdown signal and will be cancelled")
		cancel()
	}()

	globalCtx = context.WithValue(ctx, "testID", GenerateName("test-"))

	zap.S().Infof("starting tests with id %s", globalCtx.Value("testID").(string))

	go startMetricsServer(ctx)

	rc := m.Run()

	zap.S().Infof("sleeping 20s to allow metrics scraping")
	<-time.After(20 * time.Second)

	os.Exit(rc)
}

func startMetricsServer(ctx context.Context) {
	prometheus.MustRegister(InitialClusterStartupSeconds)
	prometheus.MustRegister(InitialClusterReplicasReadySeconds)
	prometheus.MustRegister(TestDurationSeconds)
	prometheus.MustRegister(stress.RequestsTotal)
	prometheus.MustRegister(stress.FailedRequestsTotal)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	server := http.Server{
		Addr:         opts.MetricsAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		timeout, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := server.Shutdown(timeout); err != nil {
			zap.S().With(zap.Error(err)).Info("error shutting down metrics server")
		}
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		zap.S().With(zap.Error(err)).Fatalf("error starting metrics server")
	}
}
