package netcup

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeJWT builds an unsigned JWT with the given payload claims JSON.
func fakeJWT(claimsJSON string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(claimsJSON))
	return header + "." + payload + ".sig"
}

func TestJWTUserID(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		want    int32
		wantErr string
	}{
		{name: "valid", token: fakeJWT(`{"id":"42","sub":"uuid"}`), want: 42},
		{name: "missing id claim", token: fakeJWT(`{"sub":"uuid"}`), wantErr: "no id claim"},
		{name: "non-numeric id", token: fakeJWT(`{"id":"abc"}`), wantErr: "parsing user ID"},
		{name: "not a jwt", token: "just-an-opaque-token", wantErr: "not a JWT"},
		{name: "bad payload encoding", token: "a.!!!.c", wantErr: "decoding JWT payload"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := jwtUserID(tt.token)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if got != tt.want {
				t.Errorf("jwtUserID = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestConnect(t *testing.T) {
	validToken := fmt.Sprintf(`{"access_token":%q,"refresh_token":"refresh-1","expires_in":300}`,
		fakeJWT(`{"id":"42"}`))

	tests := []struct {
		name      string
		tokenJSON string
		wantErr   string
	}{
		{name: "valid token", tokenJSON: validToken},
		{name: "invalid json", tokenJSON: "{not json", wantErr: "parsing OAuth token"},
		{name: "missing refresh token", tokenJSON: `{"access_token":"x"}`, wantErr: "no refresh_token"},
		{
			name:      "opaque access token",
			tokenJSON: `{"access_token":"opaque","refresh_token":"r"}`,
			wantErr:   "extracting user ID",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session, err := Connect([]byte(tt.tokenJSON), nil)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			defer session.Close()
			if session.UserID != 42 {
				t.Errorf("UserID = %d, want 42", session.UserID)
			}
			if session.API == nil {
				t.Error("API is nil")
			}
		})
	}
}

func TestConfig_Connect_EndpointOverrides(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/servers" {
			t.Errorf("unexpected API path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":7,"name":"mock-server"}]`))
	}))
	defer api.Close()

	var refreshed bool
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Errorf("unexpected IdP path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		refreshed = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"r2","expires_in":3600}`,
			fakeJWT(`{"id":"42"}`))
	}))
	defer idp.Close()

	// No obtained_at: the auth manager backdates such tokens so the first API
	// call must refresh — through the AuthURL override, not the real IdP.
	tokenJSON := fmt.Sprintf(`{"access_token":%q,"refresh_token":"r1","expires_in":300}`,
		fakeJWT(`{"id":"42"}`))

	session, err := Config{APIBaseURL: api.URL, AuthURL: idp.URL}.Connect([]byte(tokenJSON), nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	defer session.Close()

	ids, err := session.ServerIDsByName(t.Context())
	if err != nil {
		t.Fatalf("expected the session to talk to the override URLs, got: %v", err)
	}
	if ids["mock-server"] != 7 {
		t.Errorf("ServerIDsByName = %v, want mock-server:7", ids)
	}
	if !refreshed {
		t.Error("expected the backdated token to be refreshed via the AuthURL override")
	}
}

func TestIsRateLimited(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "wrapped 429",
			err:  fmt.Errorf("routing 198.51.100.1/32: route failover ipv4: unexpected status code: 429"),
			want: true,
		},
		{name: "other status", err: errors.New("unexpected status code: 500"), want: false},
		{name: "429 in unrelated text", err: errors.New("dial tcp 10.4.2.9:443: timeout"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRateLimited(tt.err); got != tt.want {
				t.Errorf("IsRateLimited = %v, want %v", got, tt.want)
			}
		})
	}
}
