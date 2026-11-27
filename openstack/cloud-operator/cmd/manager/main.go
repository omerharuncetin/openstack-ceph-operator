// Command cloud-operator runs the Kubernetes operator for TenantStoragePool,
// VirtualNetwork, and CloudWorkspace CRDs.
package main

import (
	"cmp"
	"context"
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	iaasv1alpha1 "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/api/v1alpha1"
	"github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/cloud"
	"github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/controller"
	opmetrics "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/metrics"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(iaasv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr   string
		probeAddr     string
		leaderElect   bool
		driftInterval time.Duration
		cephURL       string
		cephUser      string
		cephPass      string
		cephInsecure  bool
		osAuthURL     string
		osUser        string
		osPass        string
		osProject     string
		osDomain      string
		osRegion      string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the Prometheus metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the health/readiness probe endpoint binds to.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election for high-availability controller deployments.")
	flag.DurationVar(&driftInterval, "drift-requeue-interval", 60*time.Second, "Periodic drift detection interval for VirtualNetwork security groups.")

	flag.StringVar(&cephURL, "ceph-api-url", cmp.Or(os.Getenv("CEPH_API_URL"), "https://localhost:8443"), "Ceph MGR Dashboard REST API URL.")
	flag.StringVar(&cephUser, "ceph-api-user", cmp.Or(os.Getenv("CEPH_API_USER"), "admin"), "Ceph Dashboard username.")
	flag.StringVar(&cephPass, "ceph-api-pass", cmp.Or(os.Getenv("CEPH_API_PASS"), "admin12345!"), "Ceph Dashboard password.")
	flag.BoolVar(&cephInsecure, "ceph-insecure", true, "Skip TLS verification for self-signed Ceph Dashboard certs.")

	flag.StringVar(&osAuthURL, "os-auth-url", cmp.Or(os.Getenv("OS_AUTH_URL"), "http://localhost:5000/v3"), "OpenStack Keystone v3 endpoint.")
	flag.StringVar(&osUser, "os-username", cmp.Or(os.Getenv("OS_USERNAME"), "admin"), "OpenStack username.")
	flag.StringVar(&osPass, "os-password", cmp.Or(os.Getenv("OS_PASSWORD"), "admin12345!"), "OpenStack password.")
	flag.StringVar(&osProject, "os-project-name", cmp.Or(os.Getenv("OS_PROJECT_NAME"), "admin"), "OpenStack project/tenant name.")
	flag.StringVar(&osDomain, "os-domain-name", cmp.Or(os.Getenv("OS_USER_DOMAIN_NAME"), "Default"), "OpenStack domain name.")
	flag.StringVar(&osRegion, "os-region-name", cmp.Or(os.Getenv("OS_REGION_NAME"), "RegionOne"), "OpenStack region name.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	opmetrics.Register()

	initCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cloudClients, err := cloud.NewClients(initCtx, cloud.Config{
		CephAPIURL:             cephURL,
		CephUsername:           cephUser,
		CephPassword:           cephPass,
		CephInsecureSkipVerify: cephInsecure,
		OSAuthURL:              osAuthURL,
		OSUsername:             osUser,
		OSPassword:             osPass,
		OSProject:              osProject,
		OSDomain:               osDomain,
		OSRegionName:           osRegion,
	})
	cancel()
	if err != nil {
		setupLog.Error(err, "unable to initialize OpenStack/Ceph clients")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "iaas-cloud-operator-lock.cloud.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create controller manager")
		os.Exit(1)
	}

	if err := (&controller.TenantStoragePoolReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Cloud:    cloudClients,
		Recorder: mgr.GetEventRecorderFor("tenantstoragepool-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TenantStoragePool")
		os.Exit(1)
	}

	if err := (&controller.VirtualNetworkReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Cloud:                cloudClients,
		Recorder:             mgr.GetEventRecorderFor("virtualnetwork-controller"),
		DriftRequeueInterval: driftInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "VirtualNetwork")
		os.Exit(1)
	}

	if err := (&controller.CloudWorkspaceReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Cloud:    cloudClients,
		Recorder: mgr.GetEventRecorderFor("cloudworkspace-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CloudWorkspace")
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

	setupLog.Info("starting cloud-operator manager", "ceph_api", cephURL, "os_auth", osAuthURL, "metrics", metricsAddr)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
