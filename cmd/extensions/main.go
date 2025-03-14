// Copyright 2022 Google LLC All Rights Reserved.
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

// Extensions for the Agones System
package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agones.dev/agones/pkg"
	"agones.dev/agones/pkg/client/clientset/versioned"
	"agones.dev/agones/pkg/client/informers/externalversions"
	"agones.dev/agones/pkg/cloudproduct"
	"agones.dev/agones/pkg/fleetautoscalers"
	"agones.dev/agones/pkg/fleets"
	"agones.dev/agones/pkg/gameserverallocations"
	"agones.dev/agones/pkg/gameservers"
	"agones.dev/agones/pkg/gameserversets"
	"agones.dev/agones/pkg/metrics"
	"agones.dev/agones/pkg/util/apiserver"
	"agones.dev/agones/pkg/util/https"
	"agones.dev/agones/pkg/util/runtime"
	"agones.dev/agones/pkg/util/signals"
	"agones.dev/agones/pkg/util/webhooks"
	"github.com/heptiolabs/healthcheck"
	"github.com/pkg/errors"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"gopkg.in/natefinch/lumberjack.v2"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	enableStackdriverMetricsFlag = "stackdriver-exporter"
	stackdriverLabels            = "stackdriver-labels"
	enablePrometheusMetricsFlag  = "prometheus-exporter"
	projectIDFlag                = "gcp-project-id"
	certFileFlag                 = "cert-file"
	keyFileFlag                  = "key-file"
	numWorkersFlag               = "num-workers"
	logDirFlag                   = "log-dir"
	logLevelFlag                 = "log-level"
	logSizeLimitMBFlag           = "log-size-limit-mb"
	allocationBatchWaitTime      = "allocation-batch-wait-time"
	kubeconfigFlag               = "kubeconfig"
	defaultResync                = 30 * time.Second
	apiServerSustainedQPSFlag    = "api-server-qps"
	apiServerBurstQPSFlag        = "api-server-qps-burst"
	readinessShutdownDuration    = "readiness-shutdown-duration"
)

var (
	podReady bool
	logger   = runtime.NewLoggerWithSource("main")
)

func setupLogging(logDir string, logSizeLimitMB int) {
	logFileName := filepath.Join(logDir, "agones-extensions-"+time.Now().Format("20060102_150405")+".log")

	const maxLogSizeMB = 100
	maxBackups := (logSizeLimitMB - maxLogSizeMB) / maxLogSizeMB
	logger.WithField("filename", logFileName).WithField("numbackups", maxBackups).Info("logging to file")
	logrus.SetOutput(
		io.MultiWriter(
			logrus.StandardLogger().Out,
			&lumberjack.Logger{
				Filename:   logFileName,
				MaxSize:    maxLogSizeMB,
				MaxBackups: maxBackups,
			},
		),
	)
}

// main initializes the extensions service for Agones
func main() {
	ctx, cancelCtx := context.WithCancel(context.Background())
	ctlConf := parseEnvFlags()

	if ctlConf.LogDir != "" {
		setupLogging(ctlConf.LogDir, ctlConf.LogSizeLimitMB)
	}

	logger.WithField("logLevel", ctlConf.LogLevel).Info("Setting LogLevel configuration")
	level, err := logrus.ParseLevel(strings.ToLower(ctlConf.LogLevel))
	if err == nil {
		runtime.SetLevel(level)
	} else {
		logger.WithError(err).Info("Unable to parse loglevel, using the default loglevel - Info")
		runtime.SetLevel(logrus.InfoLevel)
	}

	logger.WithField("version", pkg.Version).WithField("featureGates", runtime.EncodeFeatures()).
		WithField("ctlConf", ctlConf).Info("starting extensions operator...")

	// if the kubeconfig fails BuildConfigFromFlags will try in cluster config
	clientConf, err := clientcmd.BuildConfigFromFlags("", ctlConf.KubeConfig)
	if err != nil {
		logger.WithError(err).Fatal("Could not create in cluster config")
	}

	clientConf.QPS = float32(ctlConf.APIServerSustainedQPS)
	clientConf.Burst = ctlConf.APIServerBurstQPS

	kubeClient, err := kubernetes.NewForConfig(clientConf)
	if err != nil {
		logger.WithError(err).Fatal("Could not create the kubernetes clientset")
	}

	agonesClient, err := versioned.NewForConfig(clientConf)
	if err != nil {
		logger.WithError(err).Fatal("Could not create the agones api clientset")
	}

	controllerHooks, err := cloudproduct.NewFromFlag(ctx, kubeClient)
	if err != nil {
		logger.WithError(err).Fatal("Could not initialize cloud product")
	}
	// https server and the items that share the Mux for routing
	httpsServer := https.NewServer(ctlConf.CertFile, ctlConf.KeyFile)
	wh := webhooks.NewWebHook(httpsServer.Mux)
	api := apiserver.NewAPIServer(httpsServer.Mux)

	agonesInformerFactory := externalversions.NewSharedInformerFactory(agonesClient, defaultResync)
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, defaultResync)

	server := &httpServer{}
	var health healthcheck.Handler

	// Stackdriver metrics
	if ctlConf.Stackdriver {
		sd, err := metrics.RegisterStackdriverExporter(ctlConf.GCPProjectID, ctlConf.StackdriverLabels)
		if err != nil {
			logger.WithError(err).Fatal("Could not register stackdriver exporter")
		}
		// It is imperative to invoke flush before your main function exits
		defer sd.Flush()
	}

	// Prometheus metrics
	if ctlConf.PrometheusMetrics {
		registry := prom.NewRegistry()
		metricHandler, err := metrics.RegisterPrometheusExporter(registry)
		if err != nil {
			logger.WithError(err).Fatal("Could not register prometheus exporter in extensions")
		}
		server.Handle("/metrics", metricHandler)
		health = healthcheck.NewMetricsHandler(registry, "agones")
	} else {
		logger.Info("Not registaring prometheus metrics")
		health = healthcheck.NewHandler()
	}

	podReady = true
	health.AddReadinessCheck("agones-extensions", func() error {
		if !podReady {
			return errors.New("asked to shut down, failed readiness check")
		}
		return nil
	})

	signals.NewSigTermHandler(func() {
		logger.Info("Pod shutdown has been requested, failing readiness check")
		podReady = false
		time.Sleep(ctlConf.ReadinessShutdownDuration)
		cancelCtx()
		logger.Infof("Readiness shutdown duration has passed, exiting pod")
		os.Exit(0)
	})

	// If we are using Prometheus only exporter we can make reporting more often,
	// every 1 seconds, if we are using Stackdriver we would use 60 seconds reporting period,
	// which is a requirements of Stackdriver, otherwise most of time series would be invalid for Stackdriver
	metrics.SetReportingPeriod(ctlConf.PrometheusMetrics, ctlConf.Stackdriver)

	server.Handle("/", health)

	gsCounter := gameservers.NewPerNodeCounter(kubeInformerFactory, agonesInformerFactory)

	gasExtensions := gameserverallocations.NewExtensions(api, health, gsCounter, kubeClient, kubeInformerFactory,
		agonesClient, agonesInformerFactory, 10*time.Second, 30*time.Second, ctlConf.AllocationBatchWaitTime)

	gameservers.NewExtensions(controllerHooks, wh)
	gameserversets.NewExtensions(controllerHooks, wh)
	fleets.NewExtensions(controllerHooks, wh)
	fleetautoscalers.NewExtensions(wh)

	kubeInformerFactory.Start(ctx.Done())
	agonesInformerFactory.Start(ctx.Done())

	for _, r := range []runner{httpsServer, gasExtensions, server} {
		go func(rr runner) {
			if runErr := rr.Run(ctx, ctlConf.NumWorkers); runErr != nil {
				logger.WithError(runErr).Fatalf("could not start runner: %T", rr)
			}
		}(r)
	}

	<-ctx.Done()
	logger.Info("Shut down agones extensions")
}

func parseEnvFlags() config {
	exec, err := os.Executable()
	if err != nil {
		logger.WithError(err).Fatal("Could not get executable path")
	}

	base := filepath.Dir(exec)
	viper.SetDefault(certFileFlag, filepath.Join(base, "certs", "server.crt"))
	viper.SetDefault(keyFileFlag, filepath.Join(base, "certs", "server.key"))
	viper.SetDefault(allocationBatchWaitTime, 500*time.Millisecond)

	viper.SetDefault(enablePrometheusMetricsFlag, true)
	viper.SetDefault(enableStackdriverMetricsFlag, false)
	viper.SetDefault(stackdriverLabels, "")

	viper.SetDefault(projectIDFlag, "")
	viper.SetDefault(numWorkersFlag, 64)
	viper.SetDefault(apiServerSustainedQPSFlag, 100)
	viper.SetDefault(apiServerBurstQPSFlag, 200)
	viper.SetDefault(logDirFlag, "")
	viper.SetDefault(logLevelFlag, "Info")
	viper.SetDefault(logSizeLimitMBFlag, 10000) // 10 GB, will be split into 100 MB chunks

	pflag.String(keyFileFlag, viper.GetString(keyFileFlag), "Optional. Path to the key file")
	pflag.String(certFileFlag, viper.GetString(certFileFlag), "Optional. Path to the crt file")
	pflag.String(kubeconfigFlag, viper.GetString(kubeconfigFlag), "Optional. kubeconfig to run the controller out of the cluster. Only use it for debugging as webhook won't works.")

	pflag.Bool(enablePrometheusMetricsFlag, viper.GetBool(enablePrometheusMetricsFlag), "Flag to activate metrics of Agones. Can also use PROMETHEUS_EXPORTER env variable.")
	pflag.Bool(enableStackdriverMetricsFlag, viper.GetBool(enableStackdriverMetricsFlag), "Flag to activate stackdriver monitoring metrics for Agones. Can also use STACKDRIVER_EXPORTER env variable.")
	pflag.String(stackdriverLabels, viper.GetString(stackdriverLabels), "A set of default labels to add to all stackdriver metrics generated. By default metadata are automatically added using Kubernetes API and GCP metadata enpoint.")

	pflag.String(projectIDFlag, viper.GetString(projectIDFlag), "GCP ProjectID used for Stackdriver, if not specified ProjectID from Application Default Credentials would be used. Can also use GCP_PROJECT_ID env variable.")
	pflag.Int32(numWorkersFlag, 64, "Number of controller workers per resource type")
	pflag.Int32(apiServerSustainedQPSFlag, 100, "Maximum sustained queries per second to send to the API server")
	pflag.Int32(apiServerBurstQPSFlag, 200, "Maximum burst queries per second to send to the API server")
	pflag.String(logDirFlag, viper.GetString(logDirFlag), "If set, store logs in a given directory.")
	pflag.Int32(logSizeLimitMBFlag, 1000, "Log file size limit in MB")
	pflag.String(logLevelFlag, viper.GetString(logLevelFlag), "Agones Log level")
	pflag.Duration(allocationBatchWaitTime, viper.GetDuration(allocationBatchWaitTime), "Flag to configure the waiting period between allocations batches")
	pflag.Duration(readinessShutdownDuration, viper.GetDuration(readinessShutdownDuration), "Time in seconds for SIGTERM handler to sleep for.")
	cloudproduct.BindFlags()
	runtime.FeaturesBindFlags()
	pflag.Parse()

	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	runtime.Must(viper.BindEnv(keyFileFlag))
	runtime.Must(viper.BindEnv(certFileFlag))
	runtime.Must(viper.BindEnv(kubeconfigFlag))

	runtime.Must(viper.BindEnv(enablePrometheusMetricsFlag))
	runtime.Must(viper.BindEnv(enableStackdriverMetricsFlag))
	runtime.Must(viper.BindEnv(stackdriverLabels))

	runtime.Must(viper.BindEnv(projectIDFlag))
	runtime.Must(viper.BindEnv(numWorkersFlag))
	runtime.Must(viper.BindEnv(apiServerSustainedQPSFlag))
	runtime.Must(viper.BindEnv(apiServerBurstQPSFlag))
	runtime.Must(viper.BindEnv(logLevelFlag))
	runtime.Must(viper.BindEnv(logDirFlag))
	runtime.Must(viper.BindEnv(logSizeLimitMBFlag))
	runtime.Must(viper.BindEnv(allocationBatchWaitTime))
	runtime.Must(viper.BindPFlags(pflag.CommandLine))
	runtime.Must(viper.BindEnv(readinessShutdownDuration))
	runtime.Must(cloudproduct.BindEnv())
	runtime.Must(runtime.FeaturesBindEnv())
	runtime.Must(runtime.ParseFeaturesFromEnv())

	return config{
		KeyFile:      viper.GetString(keyFileFlag),
		CertFile:     viper.GetString(certFileFlag),
		KubeConfig:   viper.GetString(kubeconfigFlag),
		GCPProjectID: viper.GetString(projectIDFlag),

		PrometheusMetrics: viper.GetBool(enablePrometheusMetricsFlag),
		Stackdriver:       viper.GetBool(enableStackdriverMetricsFlag),
		StackdriverLabels: viper.GetString(stackdriverLabels),

		NumWorkers:                int(viper.GetInt32(numWorkersFlag)),
		APIServerSustainedQPS:     int(viper.GetInt32(apiServerSustainedQPSFlag)),
		APIServerBurstQPS:         int(viper.GetInt32(apiServerBurstQPSFlag)),
		LogDir:                    viper.GetString(logDirFlag),
		LogLevel:                  viper.GetString(logLevelFlag),
		LogSizeLimitMB:            int(viper.GetInt32(logSizeLimitMBFlag)),
		AllocationBatchWaitTime:   viper.GetDuration(allocationBatchWaitTime),
		ReadinessShutdownDuration: viper.GetDuration(readinessShutdownDuration),
	}
}

// config stores all required configuration to create a game server extensions.
type config struct {
	KeyFile    string
	CertFile   string
	KubeConfig string

	PrometheusMetrics bool
	Stackdriver       bool
	StackdriverLabels string

	GCPProjectID              string
	NumWorkers                int
	APIServerSustainedQPS     int
	APIServerBurstQPS         int
	LogDir                    string
	LogLevel                  string
	LogSizeLimitMB            int
	AllocationBatchWaitTime   time.Duration
	ReadinessShutdownDuration time.Duration
}

type runner interface {
	Run(ctx context.Context, workers int) error
}

type httpServer struct {
	http.ServeMux
}

func (h *httpServer) Run(_ context.Context, _ int) error {
	logger.Info("Starting http server...")
	srv := &http.Server{
		Addr:    ":8080",
		Handler: h,
	}
	defer srv.Close() // nolint: errcheck

	if err := srv.ListenAndServe(); err != nil {
		if err == http.ErrServerClosed {
			logger.WithError(err).Info("http server closed")
		} else {
			wrappedErr := errors.Wrap(err, "Could not listen on :8080")
			runtime.HandleError(logger.WithError(wrappedErr), wrappedErr)
		}
	}
	return nil
}
