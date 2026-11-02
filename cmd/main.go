/*
SPDX-License-Identifier: MIT-0
*/

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"strings"
	"time"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/go-logr/logr"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
	"github.com/stanimir-kozarev/vcf-salt-operator/internal/controller"
	"github.com/stanimir-kozarev/vcf-salt-operator/internal/raasconn"
	"github.com/stanimir-kozarev/vcf-salt-operator/pkg/salt"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(saltv1alpha1.AddToScheme(scheme))
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
	var maxConcurrentReconciles int
	var defaultConnection raasconn.Default
	var enrolNamespaces, enrolSelector, enrolExclude, enrolKeyAcceptance string
	var enrolAll bool
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
	flag.IntVar(&maxConcurrentReconciles, "max-concurrent-reconciles", 5,
		"Maximum number of VirtualMachine reconciles that can run concurrently.")
	opts := zap.Options{
		Development: false,
	}
	flag.StringVar(&defaultConnection.SecretName, raasconn.DefaultConnectionFlag, "",
		"Secret in the operator's namespace holding the default RaaS connection (url, masterID, username, "+
			"password, optional skipTLSVerify). Used by every SaltKeyConfig without spec.credentialsSecret.")
	flag.StringVar(&defaultConnection.Namespace, "default-connection-namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace of the default connection Secret. Defaults to the POD_NAMESPACE environment variable.")
	flag.StringVar(&enrolNamespaces, "enrol-namespaces", "",
		"Comma-separated namespaces the operator enrols itself, with a SaltKeyConfig using the default connection.")
	flag.StringVar(&enrolSelector, "enrol-namespace-selector", "",
		"Label selector for namespaces the operator enrols itself.")
	flag.BoolVar(&enrolAll, "enrol-all-namespaces", false,
		"Enrol every namespace not excluded by --enrol-exclude-prefixes.")
	flag.StringVar(&enrolExclude, "enrol-exclude-prefixes", strings.Join(controller.DefaultEnrolmentExcludePrefixes, ","),
		"Comma-separated namespace name prefixes never enrolled automatically.")
	flag.StringVar(&enrolKeyAcceptance, "enrol-key-acceptance", saltv1alpha1.KeyAcceptanceDiscovery,
		"keyAcceptance of the SaltKeyConfigs the operator enrols, discovery or preseed.")
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

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

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

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
		LeaderElectionID:       "21b3ab69.vcf.io",
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&controller.SaltKeyConfigReconciler{
		Client:            mgr.GetClient(),
		DefaultConnection: defaultConnection,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "SaltKeyConfig")
		os.Exit(1)
	}

	if err := (&controller.VirtualMachineReconciler{
		Client: mgr.GetClient(),
		SaltClientFactory: func(raasURL, username, password, masterID string, skipTLS bool, log logr.Logger) salt.Client {
			return salt.NewRaaSClient(raasURL, username, password, masterID, skipTLS, log)
		},
		//nolint:staticcheck // TODO: migrate to events.EventRecorder
		Recorder:                mgr.GetEventRecorderFor("virtualmachine-controller"),
		MaxConcurrentReconciles: maxConcurrentReconciles,
		DefaultConnection:       defaultConnection,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "VirtualMachine")
		os.Exit(1)
	}
	enrolment := controller.Enrolment{
		Namespaces:      splitList(enrolNamespaces),
		All:             enrolAll,
		ExcludePrefixes: append(splitList(enrolExclude), defaultConnection.Namespace),
		KeyAcceptance:   enrolKeyAcceptance,
	}
	if enrolKeyAcceptance != saltv1alpha1.KeyAcceptanceDiscovery &&
		enrolKeyAcceptance != saltv1alpha1.KeyAcceptancePreseed {
		setupLog.Error(nil, "Invalid --enrol-key-acceptance, want discovery or preseed", "value", enrolKeyAcceptance)
		os.Exit(1)
	}
	if enrolSelector != "" {
		sel, err := labels.Parse(enrolSelector)
		if err != nil {
			setupLog.Error(err, "Invalid --enrol-namespace-selector")
			os.Exit(1)
		}
		enrolment.Selector = sel
	}
	if enrolment.Enabled() {
		if !defaultConnection.Configured() {
			setupLog.Info("Automatic enrolment is on without a default RaaS connection, " +
				"enrolled namespaces will report DefaultConnectionNotConfigured")
		}
		if err := (&controller.EnrolmentReconciler{
			Client:    mgr.GetClient(),
			Enrolment: enrolment,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create controller", "controller", "Enrolment")
			os.Exit(1)
		}
	} else if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		n, err := controller.ReleaseEnrolled(ctx, mgr.GetClient())
		if err != nil {
			setupLog.Error(err, "Could not remove the SaltKeyConfigs automatic enrolment created", "removed", n)
			return nil
		}
		setupLog.Info("Automatic enrolment is off, removed the SaltKeyConfigs it created", "removed", n)
		return nil
	})); err != nil {
		setupLog.Error(err, "Failed to add the enrolment release")
		os.Exit(1)
	}
	setupLog.Info("Automatic enrolment", "enabled", enrolment.Enabled(),
		"namespaces", enrolment.Namespaces, "all", enrolment.All, "selector", enrolSelector,
		"keyAcceptance", enrolment.KeyAcceptance)
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	logDefaultConnection(mgr.GetAPIReader(), defaultConnection)
	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

func logDefaultConnection(r client.Reader, def raasconn.Default) {
	if !def.Configured() {
		setupLog.Info("Default RaaS connection", "configured", false)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := def.Load(ctx, r)
	if err != nil {
		setupLog.Info("Default RaaS connection is not usable yet, SaltKeyConfigs without their own credentials wait for it",
			"namespace", def.Namespace, "secret", def.SecretName, "reason", err.Error())
		return
	}
	setupLog.Info("Default RaaS connection", "namespace", def.Namespace, "secret", def.SecretName,
		"url", conn.URL, "masterID", conn.MasterID, "skipTLSVerify", conn.SkipTLSVerify)
}

func splitList(v string) []string {
	var out []string
	for item := range strings.SplitSeq(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
