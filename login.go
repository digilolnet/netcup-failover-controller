package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/digilolnet/go-netcup-scp/pkg/scp/auth"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/digilolnet/netcup-failover-controller/internal/netcup"
)

// deviceAuthorizer is the subset of *auth.Manager used by login, so tests can
// stub the identity provider.
type deviceAuthorizer interface {
	InitiateDeviceAuth(ctx context.Context) (*auth.DeviceAuthResponse, error)
	PollForToken(ctx context.Context, deviceCode string, interval time.Duration) (*auth.TokenResponse, error)
}

// runLogin performs the OAuth2 device flow against netcup SCP and stores the
// resulting token in the credentials Secret the controller reads. It works
// with a local kubeconfig or in-cluster (e.g. via `kubectl exec`).
func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	secretRef := fs.String("secret", "netcup-system/netcup-credentials",
		"namespace/name of the Secret to store the OAuth token in")
	_ = fs.Parse(args) // ExitOnError: Parse never returns a non-nil error

	namespace, name, err := parseSecretRef(*secretRef)
	if err != nil {
		return err
	}

	ctx := ctrl.SetupSignalHandler()

	cl, err := client.New(ctrl.GetConfigOrDie(), client.Options{})
	if err != nil {
		return fmt.Errorf("connecting to the cluster: %w", err)
	}

	mgr := auth.NewManager()
	defer mgr.Close()

	token, err := deviceLogin(ctx, mgr, os.Stdout)
	if err != nil {
		return err
	}

	tokenJSON, err := json.Marshal(token) // #nosec G117 -- the token is intentionally serialized for storage in a K8s Secret
	if err != nil {
		return fmt.Errorf("serializing token: %w", err)
	}
	if err := storeToken(ctx, cl, namespace, name, tokenJSON); err != nil {
		return err
	}
	fmt.Printf("Logged in. Token stored in Secret %s/%s.\n", namespace, name)
	return nil
}

func parseSecretRef(ref string) (namespace, name string, err error) {
	namespace, name, ok := strings.Cut(ref, "/")
	if !ok || namespace == "" || name == "" {
		return "", "", fmt.Errorf("--secret must be namespace/name, got %q", ref)
	}
	return namespace, name, nil
}

// deviceLogin walks the user through the device flow: print the verification
// URL, then poll until the login completes in the browser.
func deviceLogin(ctx context.Context, idp deviceAuthorizer, out io.Writer) (*auth.TokenResponse, error) {
	da, err := idp.InitiateDeviceAuth(ctx)
	if err != nil {
		return nil, fmt.Errorf("initiating device authorization: %w", err)
	}

	uri := da.VerificationURIComplete
	if uri == "" {
		uri = da.VerificationURI
	}
	fmt.Fprintf(out, "Open this URL in your browser and log in to netcup SCP:\n\n  %s\n\n", uri)
	if da.VerificationURIComplete == "" && da.UserCode != "" {
		fmt.Fprintf(out, "When asked, enter the code: %s\n\n", da.UserCode)
	}
	if da.ExpiresIn > 0 {
		fmt.Fprintf(out, "Waiting for login (link expires in %s)...\n", (time.Duration(da.ExpiresIn) * time.Second).Round(time.Second))
	} else {
		fmt.Fprintln(out, "Waiting for login...")
	}

	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	token, err := idp.PollForToken(ctx, da.DeviceCode, interval)
	if err != nil {
		return nil, fmt.Errorf("waiting for device authorization: %w", err)
	}
	return token, nil
}

// storeToken creates the credentials Secret or updates its token key in
// place, preserving any other keys.
func storeToken(ctx context.Context, cl client.Client, namespace, name string, tokenJSON []byte) error {
	var secret corev1.Secret
	err := cl.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret)
	switch {
	case apierrors.IsNotFound(err):
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Data:       map[string][]byte{netcup.TokenSecretKey: tokenJSON},
		}
		if err := cl.Create(ctx, &secret); err != nil {
			return fmt.Errorf("creating secret %s/%s: %w", namespace, name, err)
		}
	case err != nil:
		return fmt.Errorf("reading secret %s/%s: %w", namespace, name, err)
	default:
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[netcup.TokenSecretKey] = tokenJSON
		if err := cl.Update(ctx, &secret); err != nil {
			return fmt.Errorf("updating secret %s/%s: %w", namespace, name, err)
		}
	}
	return nil
}
