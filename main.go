package main

import (
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
	ctrl.SetLogger(zap.New())

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         true,
		LeaderElectionID:       "netcup-failover-controller.digilol.net",
		HealthProbeBindAddress: ":8081",
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

	if err := (&controller.FailoverIPReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		SOAP:          netcup.NewSOAPClient(),
		RetryInterval: 3 * time.Second,
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
