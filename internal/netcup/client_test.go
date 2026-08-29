package netcup

import (
	"encoding/base64"
	"errors"
	"fmt"
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
