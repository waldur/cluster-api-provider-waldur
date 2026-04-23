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
	"flag"
	"net/http"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	infrastructurev1beta2 "github.com/waldur/cluster-api-provider-waldur/api/v1beta2"
	"github.com/waldur/cluster-api-provider-waldur/internal/controller"
	vaultpkg "github.com/waldur/cluster-api-provider-waldur/internal/vault"

	// +kubebuilder:scaffold:imports

	waldurclient "github.com/waldur/go-client"
	corev1 "k8s.io/api/core/v1"
	clusterv1beta2 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterv1beta2.AddToScheme(scheme))
	utilruntime.Must(infrastructurev1beta2.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func initWaldurClient(apiUrl string, apiToken string) (*waldurclient.ClientWithResponses, error) {
	hc := http.Client{}
	auth, err := waldurclient.NewTokenAuth(apiToken)
	if err != nil {
		setupLog.Error(err, "Error while creating token auth")
		return nil, err
	}

	client, err := waldurclient.NewClientWithResponses(
		apiUrl,
		waldurclient.WithHTTPClient(&hc),
		waldurclient.WithRequestEditorFn(auth.Intercept),
	)
	if err != nil {
		setupLog.Error(err, "Error creating Waldur client")
		return nil, err
	}

	return client, nil
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
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/server
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
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/metrics/filters#WithAuthenticationAndAuthorization
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
		LeaderElectionID:       "c2cd5ec4.cluster.waldur.com",
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

	waldurApiUrl := os.Getenv("WALDUR_API_URL")
	if waldurApiUrl == "" {
		setupLog.Info("missing required env WALDUR_API_URL")
		os.Exit(1)
	}

	waldurApiToken := os.Getenv("WALDUR_API_TOKEN")
	if waldurApiToken == "" {
		setupLog.Info("missing required env WALDUR_API_TOKEN")
		os.Exit(1)
	}

	waldur, err := initWaldurClient(waldurApiUrl, waldurApiToken)
	if err != nil {
		os.Exit(1)
	}

	// Determine the namespace the controller runs in.
	// Set via the Downward API in the Deployment spec; fallback to the default install namespace.
	operatorNamespace := os.Getenv("POD_NAMESPACE")
	if operatorNamespace == "" {
		operatorNamespace = "cluster-api-provider-waldur-system"
	}

	// Load optional static OS cloud-init base template from a ConfigMap.
	var baseTemplate []byte
	baseTemplateConfigMap := os.Getenv("BASE_TEMPLATE_CONFIGMAP")
	if baseTemplateConfigMap != "" {
		cm := &corev1.ConfigMap{}
		if err := mgr.GetAPIReader().Get(context.Background(), types.NamespacedName{
			Namespace: operatorNamespace,
			Name:      baseTemplateConfigMap,
		}, cm); err != nil {
			setupLog.Error(err, "Failed to load base template ConfigMap", "name", baseTemplateConfigMap)
			os.Exit(1)
		}
		if data, ok := cm.Data["cloud-init.yaml"]; ok {
			baseTemplate = []byte(data)
			setupLog.Info("Loaded base cloud-init template", "configmap", baseTemplateConfigMap, "bytes", len(baseTemplate))
		} else {
			setupLog.Info("ConfigMap has no cloud-init.yaml key, base template not loaded", "configmap", baseTemplateConfigMap)
		}
	}

	// Initialize Vault client (optional — disabled when VAULT_ADDR is unset).
	// Auth method selection:
	//   - Default (AppRole): reads role_id/secret_id from the K8s Secret named by VAULT_APPROLE_SECRET
	//     (default Secret name: "waldur-vault-approle"). Works across network boundaries.
	//   - Kubernetes auth (opt-in): set VAULT_K8S_ROLE. Requires Vault to reach the cluster's
	//     Kubernetes API server on port 6443. VAULT_K8S_ROLE and VAULT_APPROLE_SECRET are mutually exclusive.
	var vaultClient vaultpkg.Client
	vaultAddr := os.Getenv("VAULT_ADDR")
	if vaultAddr != "" {
		vaultK8sRole := os.Getenv("VAULT_K8S_ROLE")
		vaultAppRoleSecret := os.Getenv("VAULT_APPROLE_SECRET")
		if vaultAppRoleSecret == "" {
			vaultAppRoleSecret = "waldur-vault-approle"
		}

		if vaultK8sRole != "" {
			// Kubernetes auth — opt-in when VAULT_K8S_ROLE is explicitly set.
			vc, err := vaultpkg.NewClient(vaultpkg.Config{
				Addr: vaultAddr,
				Role: vaultK8sRole,
			})
			if err != nil {
				setupLog.Error(err, "Failed to initialize Vault client (kubernetes auth)", "addr", vaultAddr, "role", vaultK8sRole)
				os.Exit(1)
			}
			vaultClient = vc
			setupLog.Info("Vault client initialized (kubernetes auth)", "addr", vaultAddr, "role", vaultK8sRole)
		} else {
			// AppRole auth — default. Read credentials from a K8s Secret in the operator namespace.
			secret := &corev1.Secret{}
			if err := mgr.GetAPIReader().Get(context.Background(), types.NamespacedName{
				Namespace: operatorNamespace,
				Name:      vaultAppRoleSecret,
			}, secret); err != nil {
				setupLog.Error(
					err,
					"Failed to read Vault AppRole secret",
					"namespace",
					operatorNamespace,
					"name",
					vaultAppRoleSecret,
				)
				os.Exit(1)
			}
			roleID := string(secret.Data["role_id"])
			secretID := string(secret.Data["secret_id"])
			if roleID == "" || secretID == "" {
				setupLog.Error(nil, "Vault AppRole secret must contain non-empty role_id and secret_id", "name", vaultAppRoleSecret)
				os.Exit(1)
			}
			vc, err := vaultpkg.NewClientWithAppRole(vaultAddr, roleID, secretID)
			if err != nil {
				setupLog.Error(err, "Failed to initialize Vault client (approle auth)", "addr", vaultAddr)
				os.Exit(1)
			}
			vaultClient = vc
			setupLog.Info("Vault client initialized (approle auth)", "addr", vaultAddr, "secret", vaultAppRoleSecret)
		}
	} else {
		setupLog.Info("VAULT_ADDR not set — Vault integration disabled; RKE2 token will appear in user_data")
	}

	if err := (&controller.WaldurClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Waldur: *waldur,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "WaldurCluster")
		os.Exit(1)
	}
	if err := (&controller.WaldurMachineReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Waldur:            *waldur,
		VaultClient:       vaultClient,
		BaseTemplate:      baseTemplate,
		OperatorNamespace: operatorNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "WaldurMachine")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
