package main

import (
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	netcupv1alpha1 "github.com/digilolnet/netcup-failover-controller/api/v1alpha1"
	"github.com/digilolnet/netcup-failover-controller/internal/controller"
	"github.com/digilolnet/netcup-failover-controller/internal/netcup"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(netcupv1alpha1.AddToScheme(scheme))
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			if err := runLogin(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
				os.Exit(1)
			}
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\nusage: %s [login [--secret namespace/name]]\n", os.Args[1], os.Args[0])
			os.Exit(2)
		}
	}

	ctrl.SetLogger(zap.New())

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           scheme,
		LeaderElection:   true,
		LeaderElectionID: "netcup-failover-controller.digilol.net",
		// Explicit so the controller can also run out-of-cluster (dev/testing);
		// in-cluster this matches the install namespace.
		LeaderElectionNamespace: "netcup-system",
		HealthProbeBindAddress:  ":8081",
		// Nothing scrapes the controller; without this the metrics server
		// would listen unauthenticated on :8080.
		Metrics: metricsserver.Options{BindAddress: "0"},
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	})
	if err != nil {
		ctrl.Log.Error(err, "failed to create manager")
		os.Exit(1)
	}

	// The controller's Role on Secrets is scoped to its own namespace; the
	// reconciler rejects credentialsSecret references outside it.
	credentialsNamespace := os.Getenv("CREDENTIALS_NAMESPACE")
	if credentialsNamespace == "" {
		credentialsNamespace = "netcup-system"
	}

	if err := (&controller.FailoverIPReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Connect:              netcup.Connect,
		CredentialsNamespace: credentialsNamespace,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "failed to setup controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "failed to add healthz check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "failed to add readyz check")
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "failed to run manager")
		os.Exit(1)
	}
}
