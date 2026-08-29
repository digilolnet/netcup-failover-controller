package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digilolnet/go-netcup-scp/pkg/scp/auth"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/digilolnet/netcup-failover-controller/internal/netcup"
)

type fakeIdP struct {
	deviceAuth   *auth.DeviceAuthResponse
	initiateErr  error
	token        *auth.TokenResponse
	pollErr      error
	polledCode   string
	polledEvery  time.Duration
	initiateHits int
}

func (f *fakeIdP) InitiateDeviceAuth(context.Context) (*auth.DeviceAuthResponse, error) {
	f.initiateHits++
	return f.deviceAuth, f.initiateErr
}

func (f *fakeIdP) PollForToken(_ context.Context, deviceCode string, interval time.Duration) (*auth.TokenResponse, error) {
	f.polledCode = deviceCode
	f.polledEvery = interval
	return f.token, f.pollErr
}

func TestDeviceLogin(t *testing.T) {
	tests := []struct {
		name       string
		idp        *fakeIdP
		wantErr    string
		wantOutput []string
		wantPoll   time.Duration
	}{
		{
			name: "complete URI shown and token returned",
			idp: &fakeIdP{
				deviceAuth: &auth.DeviceAuthResponse{
					DeviceCode:              "dev-1",
					UserCode:                "ABCD-EFGH",
					VerificationURI:         "https://idp.example/device",
					VerificationURIComplete: "https://idp.example/device?user_code=ABCD-EFGH",
					ExpiresIn:               600,
					Interval:                2,
				},
				token: &auth.TokenResponse{RefreshToken: "r"},
			},
			wantOutput: []string{"https://idp.example/device?user_code=ABCD-EFGH", "expires in 10m0s"},
			wantPoll:   2 * time.Second,
		},
		{
			name: "falls back to plain URI plus code",
			idp: &fakeIdP{
				deviceAuth: &auth.DeviceAuthResponse{
					DeviceCode:      "dev-1",
					UserCode:        "ABCD-EFGH",
					VerificationURI: "https://idp.example/device",
				},
				token: &auth.TokenResponse{RefreshToken: "r"},
			},
			wantOutput: []string{"https://idp.example/device", "enter the code: ABCD-EFGH"},
			wantPoll:   5 * time.Second, // defaulted when the IdP sends no interval
		},
		{
			name:    "initiate fails",
			idp:     &fakeIdP{initiateErr: errors.New("idp down")},
			wantErr: "initiating device authorization",
		},
		{
			name: "poll fails",
			idp: &fakeIdP{
				deviceAuth: &auth.DeviceAuthResponse{DeviceCode: "dev-1", VerificationURI: "https://idp.example/device"},
				pollErr:    errors.New("expired_token"),
			},
			wantErr: "waiting for device authorization",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder
			token, err := deviceLogin(t.Context(), tt.idp, &out)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if token != tt.idp.token {
				t.Error("returned token is not the one from the IdP")
			}
			if tt.idp.polledCode != tt.idp.deviceAuth.DeviceCode {
				t.Errorf("polled device code %q, want %q", tt.idp.polledCode, tt.idp.deviceAuth.DeviceCode)
			}
			if tt.idp.polledEvery != tt.wantPoll {
				t.Errorf("poll interval = %v, want %v", tt.idp.polledEvery, tt.wantPoll)
			}
			for _, want := range tt.wantOutput {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output missing %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestStoreToken(t *testing.T) {
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "netcup-system", Name: "netcup-credentials"},
		Data:       map[string][]byte{"token": []byte("old"), "other": []byte("keep")},
	}

	tests := []struct {
		name     string
		objs     []*corev1.Secret
		wantKeys map[string]string
	}{
		{
			name:     "creates missing secret",
			wantKeys: map[string]string{"token": "new-token"},
		},
		{
			name:     "updates existing secret preserving other keys",
			objs:     []*corev1.Secret{existing},
			wantKeys: map[string]string{"token": "new-token", "other": "keep"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder()
			for _, s := range tt.objs {
				builder = builder.WithObjects(s.DeepCopy())
			}
			cl := builder.Build()

			if err := storeToken(t.Context(), cl, "netcup-system", "netcup-credentials", []byte("new-token")); err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}

			var secret corev1.Secret
			if err := cl.Get(t.Context(), types.NamespacedName{Namespace: "netcup-system", Name: "netcup-credentials"}, &secret); err != nil {
				t.Fatalf("getting secret: %v", err)
			}
			for key, want := range tt.wantKeys {
				if got := string(secret.Data[key]); got != want {
					t.Errorf("secret.Data[%q] = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestStoreToken_UsesSharedKey(t *testing.T) {
	// The login command and the controller must agree on the Secret key.
	if netcup.TokenSecretKey != "token" {
		t.Fatalf("TokenSecretKey = %q, want token", netcup.TokenSecretKey)
	}
}

func TestParseSecretRef(t *testing.T) {
	tests := []struct {
		ref           string
		wantNS, wantN string
		wantErr       bool
	}{
		{ref: "netcup-system/netcup-credentials", wantNS: "netcup-system", wantN: "netcup-credentials"},
		{ref: "no-slash", wantErr: true},
		{ref: "/name-only", wantErr: true},
		{ref: "ns-only/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			ns, name, err := parseSecretRef(tt.ref)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if ns != tt.wantNS || name != tt.wantN {
				t.Errorf("parseSecretRef = %s/%s, want %s/%s", ns, name, tt.wantNS, tt.wantN)
			}
		})
	}
}
