// Copyright 2020 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	appv1beta1 "sigs.k8s.io/application/api/v1beta1"
	"sigs.k8s.io/application/controllers"
	"sigs.k8s.io/application/logmetrics"
	"sigs.k8s.io/application/push"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	// version is overridable at build time via -ldflags "-X main.version=...".
	version = "dev"
)

// appVersion returns the controller version reported to triage in the push hello frame.
func appVersion() string { return version }

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = appv1beta1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

func main() {
	var namespace string
	var metricsAddr string
	var syncPeriod int64
	var stabilizationPeriod int64
	var enableLeaderElection bool
	var concurrentReconciles int
	var pushEndpoint, clusterName, pushTenant, pushToken, pushTokenFile, pushNamespaces string
	var pushHeartbeat int64
	var pushInsecure, pushAllowPlaintext bool
	var logMetricsEnabled bool
	var logMetricsServiceNamespace, logMetricsServiceName string
	var logMetricsPort int
	var logMetricsIntervalSeconds int64
	var logMetricsErrorThreshold int64
	var logMetricsErrorMetric, logMetricsWarnMetric, logMetricsTotalMetric string
	var logMetricsNamespaceLabel, logMetricsServiceLabel, logMetricsServiceLabelFallback string
	flag.StringVar(&namespace, "namespace", "", "Namespace within which CRD controller is running.")
	flag.StringVar(&metricsAddr, "metrics-addr", "127.0.0.1:8080", "The address the metric endpoint binds to. Defaults to loopback; expose via an authenticating proxy (e.g. kube-rbac-proxy) rather than binding to all interfaces.")
	flag.Int64Var(&syncPeriod, "sync-period", 120, "Sync every sync-period seconds.")
	flag.Int64Var(&stabilizationPeriod, "stabilization-period", 30, "Seconds to wait before transitioning an Application to Ready, to avoid flapping.")
	flag.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller app-controller. Enabling this will ensure there is only one active controller app-controller.")
	flag.IntVar(&concurrentReconciles, "concurrent-reconciles", 4,
		"Maximum number of Applications reconciled in parallel. Reconciles of different Applications are independent, so this is safe to raise.")

	// Push mode (ADR-0005): stream Applications + Warning events to a triage agent
	// over an outbound WebSocket. Off unless --push-endpoint is set.
	flag.StringVar(&pushEndpoint, "push-endpoint", "", "Triage agent WebSocket URL (wss://host/events/ws). Empty disables push mode.")
	flag.StringVar(&clusterName, "cluster-name", "", "Cluster identifier stamped into pushed events. Required when --push-endpoint is set.")
	flag.StringVar(&pushTenant, "tenant", "", "Tenant this cluster belongs to; selects the triage service graph. Required when --push-endpoint is set.")
	flag.StringVar(&pushToken, "push-token", "", "Per-tenant HMAC signing key for the handshake (prefer --push-token-file).")
	flag.StringVar(&pushTokenFile, "push-token-file", "", "Path to a file containing the per-tenant HMAC signing key.")
	flag.StringVar(&pushNamespaces, "push-namespaces", "", "Comma-separated namespaces to push; empty pushes all.")
	flag.Int64Var(&pushHeartbeat, "push-heartbeat", 20, "Heartbeat interval in seconds.")
	flag.BoolVar(&pushInsecure, "push-insecure-skip-verify", false, "Skip TLS certificate verification for a wss:// endpoint (dev only).")
	flag.BoolVar(&pushAllowPlaintext, "push-allow-plaintext", false, "Allow a plaintext ws:// endpoint (sends the bearer token unencrypted; dev/trusted-network only).")

	// Log-based metrics (ADR-0006): scrape Fluent Bit's log_to_metrics exporter and
	// forward error/warn/total counters to triage as log_metrics frames over the same
	// push connection. Off unless --log-metrics-enabled is set; a no-op if push mode
	// itself is disabled (nothing to send the frame through).
	flag.BoolVar(&logMetricsEnabled, "log-metrics-enabled", false, "Enable the log-based metrics collector (scrapes Fluent Bit, sends log_metrics frames via push mode).")
	flag.StringVar(&logMetricsServiceNamespace, "log-metrics-service-namespace", "", "Namespace of the Service fronting the Fluent Bit DaemonSet. Required when --log-metrics-enabled is set.")
	flag.StringVar(&logMetricsServiceName, "log-metrics-service-name", "triage-fluentbit", "Name of the Service fronting the Fluent Bit DaemonSet.")
	flag.IntVar(&logMetricsPort, "log-metrics-port", 2021, "Fluent Bit prometheus_exporter port on each pod.")
	flag.Int64Var(&logMetricsIntervalSeconds, "log-metrics-interval-seconds", 60, "Scrape + gate-evaluation interval in seconds.")
	flag.Int64Var(&logMetricsErrorThreshold, "log-metrics-error-threshold", 10, "Minimum error-count OR warn-count delta in one interval for a service to be reported (either counter crossing it independently qualifies).")
	flag.StringVar(&logMetricsErrorMetric, "log-metrics-error-metric", "log_metric_counter_log_errors_total", "Prometheus counter family name for error log lines, as emitted by Fluent Bit.")
	flag.StringVar(&logMetricsWarnMetric, "log-metrics-warn-metric", "log_metric_counter_log_warns_total", "Prometheus counter family name for warn log lines. Empty disables warn reporting.")
	flag.StringVar(&logMetricsTotalMetric, "log-metrics-total-metric", "log_metric_counter_log_lines_total", "Prometheus counter family name for all log lines. Empty disables total reporting.")
	flag.StringVar(&logMetricsNamespaceLabel, "log-metrics-namespace-label", "namespace", "Prometheus label key identifying the source namespace on each sample.")
	flag.StringVar(&logMetricsServiceLabel, "log-metrics-service-label", "service", "Prometheus label key identifying the source service on each sample.")
	flag.StringVar(&logMetricsServiceLabelFallback, "log-metrics-service-label-fallback", "service_fallback", "Second label key consulted when --log-metrics-service-label is absent on a sample. Empty disables the fallback lookup.")

	// Bind the zap logging flags (--zap-log-level, --zap-devel, --zap-encoder, ...) so log
	// verbosity is controllable at runtime.
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	syncPeriodD := time.Duration(int64(time.Second) * syncPeriod)
	cacheOpts := cache.Options{SyncPeriod: &syncPeriodD}
	if namespace != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{namespace: {}}
	}
	// managedFields are often 30-40% of an object's serialized size and the controller
	// never reads them; stripping them before they're committed to the informer cache
	// cuts steady-state memory noticeably across every watched GVK.
	cacheOpts.DefaultTransform = cache.TransformStripManagedFields()

	// Push mode's pusher registers GetInformer(&corev1.Event{}), which by default caches
	// EVERY Event cluster-wide — the highest-churn resource in most clusters. Scope the
	// informer itself to Warning events at the cache level; the pusher's own
	// e.Type != EventTypeWarning check in onEvent stays as defense in depth. Only set this
	// in push mode: the Event informer is never started otherwise, but be explicit rather
	// than relying on that.
	if pushEndpoint != "" {
		cacheOpts.ByObject = map[client.Object]cache.ByObject{
			&corev1.Event{}: {Field: fields.OneTermEqualSelector("type", corev1.EventTypeWarning)},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           scheme,
		Metrics:          metricsserver.Options{BindAddress: metricsAddr},
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "app-controller",
		Cache:            cacheOpts,
		// The dynamic component watches (ensureComponentWatches) already run informers
		// for every component GVK an Application declares. Without this, the default
		// controller-runtime v0.19 client bypasses the cache for unstructured
		// reads/lists, so fetchComponentListResources would issue a live LIST to the API
		// server on every single reconcile instead of reading from the informer cache.
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: true}},
	})
	if err != nil {
		setupLog.Error(err, "unable to start app-controller")
		os.Exit(1)
	}

	if err = (&controllers.ApplicationReconciler{
		Client:               mgr.GetClient(),
		Mapper:               mgr.GetRESTMapper(),
		StabilizationPeriod:  time.Duration(stabilizationPeriod) * time.Second,
		ConcurrentReconciles: concurrentReconciles,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Application")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Push mode: stream Applications + Warning events to a triage agent (ADR-0005).
	var pusher *push.Pusher
	if pushEndpoint != "" {
		if err := push.ValidateEndpoint(pushEndpoint, pushAllowPlaintext); err != nil {
			setupLog.Error(err, "invalid --push-endpoint")
			os.Exit(1)
		}
		if clusterName == "" {
			setupLog.Error(nil, "--cluster-name is required when --push-endpoint is set")
			os.Exit(1)
		}
		if pushTenant == "" {
			setupLog.Error(nil, "--tenant is required when --push-endpoint is set")
			os.Exit(1)
		}
		if pushToken == "" && pushTokenFile == "" {
			setupLog.Error(nil, "an HMAC key is required: set --push-token or --push-token-file")
			os.Exit(1)
		}
		nsList := push.ParseNamespaces(pushNamespaces)
		if len(nsList) == 0 && namespace != "" {
			nsList = []string{namespace}
		}
		pusher = push.New(push.Options{
			Endpoint:     pushEndpoint,
			ClusterName:  clusterName,
			Tenant:       pushTenant,
			Token:        pushToken,
			TokenFile:    pushTokenFile,
			Namespaces:   nsList,
			AgentVersion: appVersion(),
			Heartbeat:    time.Duration(pushHeartbeat) * time.Second,
			InsecureTLS:  pushInsecure,
			LogMetrics:   logMetricsEnabled,
		}, mgr, ctrl.Log)
		if err := mgr.Add(pusher); err != nil {
			setupLog.Error(err, "unable to add push runnable")
			os.Exit(1)
		}
		setupLog.Info("push mode enabled", "endpoint", pushEndpoint, "cluster", clusterName, "namespaces", nsList)
	}

	// Log-based metrics (ADR-0006): scrape Fluent Bit + forward via the pusher above.
	// logmetrics.New is a no-op (returns nil) when either disabled or push mode itself
	// is off. Pass an explicit untyped nil (not the *push.Pusher variable, even though
	// it may itself be nil) when push mode is disabled — a nil *push.Pusher stored in
	// the Sender interface would NOT compare equal to nil inside New (Go's typed-nil-
	// interface trap), so the collector would start and scrape pointlessly.
	if logMetricsEnabled {
		if logMetricsServiceNamespace == "" {
			setupLog.Error(nil, "--log-metrics-service-namespace is required when --log-metrics-enabled is set")
			os.Exit(1)
		}
		var sender logmetrics.Sender
		if pusher != nil {
			sender = pusher
		}
		collector := logmetrics.New(logmetrics.Options{
			ServiceNamespace:     logMetricsServiceNamespace,
			ServiceName:          logMetricsServiceName,
			Port:                 logMetricsPort,
			Interval:             time.Duration(logMetricsIntervalSeconds) * time.Second,
			ErrorThreshold:       logMetricsErrorThreshold,
			ErrorMetric:          logMetricsErrorMetric,
			WarnMetric:           logMetricsWarnMetric,
			TotalMetric:          logMetricsTotalMetric,
			NamespaceLabel:       logMetricsNamespaceLabel,
			ServiceLabel:         logMetricsServiceLabel,
			ServiceLabelFallback: logMetricsServiceLabelFallback,
		}, mgr, sender, ctrl.Log)
		if collector == nil {
			setupLog.Info("log-metrics collector not started: push mode is disabled, so there is nowhere to send frames")
		} else if err := mgr.Add(collector); err != nil {
			setupLog.Error(err, "unable to add log-metrics collector runnable")
			os.Exit(1)
		} else {
			setupLog.Info("log-metrics collector enabled",
				"service", logMetricsServiceNamespace+"/"+logMetricsServiceName, "port", logMetricsPort,
				"interval", logMetricsIntervalSeconds, "errorThreshold", logMetricsErrorThreshold)
		}
	}

	setupLog.Info("starting app-controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running app-controller")
		os.Exit(1)
	}
}
