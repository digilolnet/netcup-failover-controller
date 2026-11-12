// Command netcup-failover-controller routes netcup failover IPs to healthy
// Kubernetes nodes. Run without arguments it starts the controller manager;
// `login` runs the OAuth2 device flow that seeds the credentials Secret.
package main

import (
	"cmp"
	"fmt"
	"os"
	"time"

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

// version is the release tag, injected at build time with
// -ldflags "-X main.version=...".
var version = "dev"

var scheme = runtime.NewScheme()

// controllerNamespace is the namespace the controller is installed in. It
// holds the leader-election lease and the credentials Secrets, matching the
// namespaced Role the controller has on Secrets. In-cluster it comes from the
// downward API; the fallback covers out-of-cluster runs (dev, local login).
func controllerNamespace() string {
	return cmp.Or(os.Getenv("CREDENTIALS_NAMESPACE"), "netcup-system")
}

// defaultResyncInterval is how often routed groups are compared against
// netcup's actual routing. The check only reads, so it does not spend the
// routing rate limit.
const defaultResyncInterval = 5 * time.Minute

// resyncInterval reads RESYNC_INTERVAL (a Go duration, e.g. "5m"). Unset
// means the default; "0" disables the periodic check.
func resyncInterval() (time.Duration, error) {
	raw := os.Getenv("RESYNC_INTERVAL")
	if raw == "" {
		return defaultResyncInterval, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parsing RESYNC_INTERVAL: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("RESYNC_INTERVAL must not be negative, got %s", raw)
	}
	return d, nil
}

func init() {
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(netcupv1alpha1.AddToScheme(scheme))
}

const usage = "usage: %s [login [--secret namespace/name] | version | help]\n"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login":
			if err := runLogin(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
				os.Exit(1)
			}
			return
		case "version", "--version":
			fmt.Println(version)
			return
		case "help", "-h", "--help":
			fmt.Printf(usage, os.Args[0])
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n"+usage, os.Args[1], os.Args[0])
			os.Exit(2)
		}
	}

	ctrl.SetLogger(zap.New())
	namespace := controllerNamespace()
	ctrl.Log.Info("starting netcup-failover-controller", "version", version, "namespace", namespace)

	resync, err := resyncInterval()
	if err != nil {
		ctrl.Log.Error(err, "invalid configuration")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           scheme,
		LeaderElection:   true,
		LeaderElectionID: "netcup-failover-controller.digilol.net",
		// Explicit so the controller can also run out-of-cluster (dev/testing).
		LeaderElectionNamespace: namespace,
		// main exits as soon as the manager stops, so the lease can be handed
		// over immediately instead of the standby waiting for it to expire.
		LeaderElectionReleaseOnCancel: true,
		HealthProbeBindAddress:        ":8081",
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

	if err := (&controller.FailoverIPReconciler{
		Client:               mgr.GetClient(),
		Connect:              netcup.ConfigFromEnv().Connect,
		CredentialsNamespace: namespace,
		ResyncInterval:       resync,
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
