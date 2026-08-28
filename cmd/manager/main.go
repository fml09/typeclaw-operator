package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/metadata"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	typeclawv1alpha1 "github.com/fml09/typeclaw-operator/api/v1alpha1"
	"github.com/fml09/typeclaw-operator/internal/controller"
	"github.com/fml09/typeclaw-operator/internal/credential"
	"github.com/fml09/typeclaw-operator/internal/update"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(typeclawv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var brokerAddr string
	var brokerCertificate string
	var brokerPrivateKey string
	var brokerClientCA string
	var brokerTrustDomain string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address for the metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address for the health probes")
	flag.StringVar(&brokerAddr, "credential-broker-bind-address", "", "mTLS address for the typed credential broker; empty disables it")
	flag.StringVar(&brokerCertificate, "credential-broker-certificate", "", "server certificate for the typed credential broker")
	flag.StringVar(&brokerPrivateKey, "credential-broker-private-key", "", "server private key for the typed credential broker")
	flag.StringVar(&brokerClientCA, "credential-broker-client-ca", "", "client CA for SPIFFE mTLS authentication")
	flag.StringVar(&brokerTrustDomain, "credential-broker-trust-domain", credential.RunnerSPIFFETrustDomain, "SPIFFE trust domain accepted by the broker")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	metadataClient, err := metadata.NewForConfig(cfg)
	if err != nil {
		setupLog.Error(err, "unable to create metadata client")
		os.Exit(1)
	}

	if err := (&controller.TypeClawInstanceReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TypeClawInstance")
		os.Exit(1)
	}

	if err := (&controller.CredentialRequestReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		SecretMetadata: controller.KubernetesSecretMetadataReader{Client: metadataClient},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CredentialRequest")
		os.Exit(1)
	}
	if err := (&controller.NetworkPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NetworkPolicy")
		os.Exit(1)
	}
	if err := (&controller.BackupController{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Backup")
		os.Exit(1)
	}

	if brokerAddr != "" {
		broker := &credential.Broker{
			Reader:      mgr.GetClient(),
			Writer:      mgr.GetClient(),
			TrustDomain: brokerTrustDomain,
		}
		server := controller.NewCredentialBrokerServer(
			broker,
			brokerAddr,
			brokerCertificate,
			brokerPrivateKey,
			brokerClientCA,
		)
		if err := mgr.Add(server); err != nil {
			setupLog.Error(err, "unable to add credential broker")
			os.Exit(1)
		}
	}
	if err := (&controller.AutoUpdateReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Registry: &update.RegistryClient{},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AutoUpdate")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
