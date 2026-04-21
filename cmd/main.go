/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	governancev1alpha1 "github.com/agent-control-plane/aip-k8s/api/v1alpha1"
	"github.com/agent-control-plane/aip-k8s/internal/controller"
	"github.com/agent-control-plane/aip-k8s/internal/evaluation"
	"github.com/agent-control-plane/aip-k8s/internal/gc"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(governancev1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opsLockDuration := flag.Duration("ops-lock-duration", 5*time.Minute, "TTL for OpsLock leases")

	const otlpShutdownTimeout = 5 * time.Second

	// GC flags
	gcCfg := gc.DefaultGCConfig()
	flag.BoolVar(&gcCfg.Enabled, "gc-enabled", gcCfg.Enabled, "Enable the GC engine.")
	flag.DurationVar(&gcCfg.Interval, "gc-interval", gcCfg.Interval, "Time between GC cycles.")
	flag.BoolVar(&gcCfg.DryRun, "gc-dry-run", gcCfg.DryRun, "Log deletions without acting. Defaults true for safety.")
	flag.DurationVar(&gcCfg.DiagnosticHardTTL, "gc-diagnostic-hard-ttl", gcCfg.DiagnosticHardTTL,
		"Forced deletion age for AgentDiagnostics regardless of export state.")
	flag.DurationVar(&gcCfg.DiagnosticRetentionTTL, "gc-diagnostic-retention-ttl", 0,
		"Soft retention window for AgentDiagnostics. Records past this window are exported "+
			"before deletion. 0 disables soft retention (hard TTL only).")
	flag.StringVar(&gcCfg.ExportType, "gc-export-type", "none",
		"Export provider type. Valid values: none, otlp.")
	flag.StringVar(&gcCfg.OTLPEndpoint, "gc-otlp-endpoint", "",
		"OTLP gRPC endpoint for export (e.g. otel-collector:4317). "+
			"Required when --gc-export-type=otlp and soft retention is enabled.")
	flag.BoolVar(&gcCfg.OTLPInsecure, "gc-otlp-insecure", gcCfg.OTLPInsecure,
		"If true, use an insecure connection for OTLP export.")
	flag.IntVar(&gcCfg.Concurrency, "gc-export-concurrency", 5,
		"Number of concurrent export workers per resource type.")
	flag.Int64Var(&gcCfg.PageSize, "gc-page-size", gcCfg.PageSize, "Objects per list page during GC scan.")
	flag.Float64Var(&gcCfg.DeleteRatePerSec, "gc-delete-rate-per-sec", gcCfg.DeleteRatePerSec,
		"Max object deletions per second.")
	flag.IntVar(&gcCfg.SafetyMinCount, "gc-safety-min-count", gcCfg.SafetyMinCount,
		"Skip GC if total object count is below this threshold.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Validate ExportType
	if gcCfg.ExportType != "none" && gcCfg.ExportType != "otlp" {
		setupLog.Error(nil, "Invalid --gc-export-type", "type", gcCfg.ExportType)
		os.Exit(1)
	}

	// Validate opsLockDuration
	if *opsLockDuration <= 0 {
		setupLog.Error(nil, "Invalid --ops-lock-duration: must be greater than 0", "duration", *opsLockDuration)
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	ctx := ctrl.SetupSignalHandler()

	var gcExporter gc.Exporter
	if gcCfg.ExportType == "otlp" && gcCfg.DiagnosticRetentionTTL > 0 {
		if gcCfg.OTLPEndpoint == "" {
			setupLog.Error(nil, "--gc-otlp-endpoint is required when --gc-export-type=otlp and retention is enabled")
			os.Exit(1)
		}

		otlpExp, err := gc.NewOTLPExporter(ctx, gcCfg.OTLPEndpoint, gcCfg.OTLPInsecure)
		if err != nil {
			setupLog.Error(err, "Failed to create OTLP exporter")
			os.Exit(1)
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), otlpShutdownTimeout)
			defer cancel()
			if err := otlpExp.Shutdown(shutdownCtx); err != nil {
				setupLog.Error(err, "Failed to shutdown OTLP exporter")
			}
		}()
		gcExporter = otlpExp
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "fd40cfe6.aip.io",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	eval, err := evaluation.NewEvaluator()
	if err != nil {
		setupLog.Error(err, "Failed to initialize policy evaluator")
		os.Exit(1)
	}

	if err := (&controller.AgentRequestReconciler{
		Client:               mgr.GetClient(),
		APIReader:            mgr.GetAPIReader(),
		Scheme:               mgr.GetScheme(),
		OpsLockDuration:      *opsLockDuration,
		Evaluator:            eval,
		TargetContextFetcher: &evaluation.KubernetesTargetContextFetcher{Client: mgr.GetAPIReader()},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "AgentRequest")
		os.Exit(1)
	}
	if err := (&controller.SafetyPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "SafetyPolicy")
		os.Exit(1)
	}
	if err := (&controller.GovernedResourceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "GovernedResource")
		os.Exit(1)
	}
	if err := (&controller.DiagnosticAccuracyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "DiagnosticAccuracy")
		os.Exit(1)
	}

	gcMgr := &gc.GCManager{
		APIReader: mgr.GetAPIReader(),
		Client:    mgr.GetClient(),
		Config:    gcCfg,
		Now:       time.Now,
		Exporter:  gcExporter,
	}

	if err := mgr.Add(gcMgr); err != nil {
		setupLog.Error(err, "Failed to register GC manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("gc-healthz", gcMgr.Check); err != nil {
		setupLog.Error(err, "Failed to set up GC health check")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	const ReadyzTimeout = 5 * time.Second
	if err := mgr.AddReadyzCheck("readyz", func(req *http.Request) error {
		ctx, cancel := context.WithTimeout(req.Context(), ReadyzTimeout)
		defer cancel()
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return errors.New("caches did not sync")
		}
		return nil
	}); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
