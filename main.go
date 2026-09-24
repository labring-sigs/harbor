package main

import (
	"flag"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	harborv1 "github.com/dinoallo/labring-sigs-harbor/api/v1"
	"github.com/dinoallo/labring-sigs-harbor/controllers"
	"github.com/dinoallo/labring-sigs-harbor/internal/harbor"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(harborv1.AddToScheme(scheme))
}

const (
	defaultStorageLimitFlagDefault = 5 * 1024 * 1024 * 1024 // 5 GiB
)

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var enableProjectAutoProvision bool
	var ownerLabelKey string
	var defaultStorageLimit int64

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&enableProjectAutoProvision, "enable-project-auto-provision", false,
		"Enable automatic creation of HarborProject CRs for namespaces that carry the owner label key.")
	flag.StringVar(&ownerLabelKey, "owner-label-key", "user.sealos.io/owner",
		"Label key on namespaces used to determine the owner for auto-provisioned HarborProject CRs.")
	flag.Int64Var(&defaultStorageLimit, "default-storage-limit", defaultStorageLimitFlagDefault,
		"Default storage limit in bytes for auto-provisioned HarborProjects. Use -1 for unlimited.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "harbor.sealos.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Read configuration from environment variables
	harborEndpoint := os.Getenv("HARBOR_ENDPOINT")
	harborAdminUser := os.Getenv("HARBOR_ADMIN_USERNAME")
	harborAdminPass := os.Getenv("HARBOR_ADMIN_PASSWORD")
	registryHost := os.Getenv("REGISTRY_HOST")

	if harborEndpoint == "" {
		harborEndpoint = "https://core.harbor.svc:8443"
	}
	if harborAdminUser == "" {
		harborAdminUser = "admin"
	}
	if registryHost == "" {
		registryHost = "registry.sealos.io"
	}

	harborClient := harbor.NewClient(harborEndpoint, harborAdminUser, harborAdminPass)

	if err = (&controllers.HarborProjectReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		HarborClient: harborClient,
		RegistryHost: registryHost,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "HarborProject")
		os.Exit(1)
	}

	if enableProjectAutoProvision {
		setupLog.Info("project auto-provision enabled",
			"ownerLabelKey", ownerLabelKey,
			"defaultStorageLimit", defaultStorageLimit,
		)
		if err = (&controllers.ProjectAutoProvisionReconciler{
			Client:                   mgr.GetClient(),
			Scheme:                   mgr.GetScheme(),
			OwnerLabelKey:            ownerLabelKey,
			DefaultStorageLimitBytes: defaultStorageLimit,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "ProjectAutoProvision")
			os.Exit(1)
		}
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
