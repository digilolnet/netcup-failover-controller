// Package netcup connects the controller to the netcup SCP REST API via
// github.com/digilolnet/go-netcup-scp. The old SOAP webservice was shut down
// on 2026-05-01.
package netcup

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/digilolnet/go-netcup-scp/pkg/scp"
	"github.com/digilolnet/go-netcup-scp/pkg/scp/auth"
)

// TokenSecretKey is the credentials Secret key holding the OAuth2 token JSON.
// It is the contract between the controller (which reads and refreshes the
// token) and the login command (which seeds it).
const TokenSecretKey = "token"

const (
	defaultCallTimeout      = 30 * time.Second
	defaultTaskPollInterval = 2 * time.Second
	defaultTaskTimeout      = 2 * time.Minute
)

// API is the subset of *scp.Client used by the controller, as an interface so
// tests can inject a mock.
type API interface {
	ListServers(ctx context.Context, opts *scp.ListServersOptions) ([]scp.ServerListMinimal, error)
	ListFailoverIPv4(ctx context.Context, userID int32, opts *scp.ListFailoverIPsOptions) ([]scp.FailoverIPv4, error)
	ListFailoverIPv6(ctx context.Context, userID int32, opts *scp.ListFailoverIPsOptions) ([]scp.FailoverIPv6, error)
	RouteFailoverIPv4(ctx context.Context, userID, failoverID, serverID int32) (*scp.TaskInfo, error)
	RouteFailoverIPv6(ctx context.Context, userID, failoverID, serverID int32) (*scp.TaskInfo, error)
	GetTask(ctx context.Context, uuid string) (*scp.TaskInfo, error)
}

// Session is an authenticated SCP API session bound to one netcup account.
type Session struct {
	API    API
	UserID int32
	// CallTimeout bounds each individual SCP API call: neither the library's
	// HTTP client nor a reconcile context carries a deadline of its own, so
	// without it one stalled response would block the caller forever. Zero
	// means the default (30s).
	CallTimeout time.Duration
	// TaskPollInterval and TaskTimeout bound the wait for the async routing
	// tasks the SCP API returns. Zero means the defaults (2s and 2m).
	TaskPollInterval time.Duration
	TaskTimeout      time.Duration

	close func()
}

// Close releases the session's HTTP and token-refresh resources.
func (s *Session) Close() {
	if s.close != nil {
		s.close()
	}
}

// Config holds optional endpoint overrides; the zero value uses the netcup
// production endpoints.
type Config struct {
	// APIBaseURL overrides the SCP REST API base URL
	// (default https://www.servercontrolpanel.de/scp-core), e.g. for a mock
	// server or an endpoint move.
	APIBaseURL string
	// AuthURL overrides the OpenID Connect endpoint base
	// (default the netcup SCP Keycloak realm).
	AuthURL string
}

// ConfigFromEnv reads the optional endpoint overrides NETCUP_SCP_API_URL and
// NETCUP_SCP_AUTH_URL. The controller and the login command share it so both
// always talk to the same endpoints.
func ConfigFromEnv() Config {
	return Config{
		APIBaseURL: os.Getenv("NETCUP_SCP_API_URL"),
		AuthURL:    os.Getenv("NETCUP_SCP_AUTH_URL"),
	}
}

// AuthManager builds an OAuth2 manager for the configured identity provider.
// The caller owns it and must Close it.
func (c Config) AuthManager(opts ...auth.Option) *auth.Manager {
	if c.AuthURL != "" {
		opts = append(opts, auth.WithAuthURL(c.AuthURL))
	}
	return auth.NewManager(opts...)
}

// Connect builds an authenticated session from a JSON-serialized OAuth2 token
// (the TokenSecretKey value of the credentials Secret, seeded by
// `netcup-failover-controller login`). onRefresh, if non-nil, is invoked with
// the re-serialized token whenever the IdP issues a fresh one, so the caller
// can persist it before the stored refresh token expires. It runs
// synchronously on the goroutine making the API call that needed the refresh.
func (c Config) Connect(tokenJSON []byte, onRefresh func(tokenJSON []byte)) (*Session, error) {
	var tok auth.TokenResponse
	if err := json.Unmarshal(tokenJSON, &tok); err != nil {
		return nil, fmt.Errorf("parsing OAuth token: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("OAuth token has no refresh_token")
	}
	userID, err := jwtUserID(tok.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("extracting user ID from access token: %w", err)
	}

	// Sessions live for a single reconcile, so the library's background
	// refresh timer is off: tokens are refreshed on demand by the API call
	// that needs one, and onRefresh never fires on another goroutine or after
	// the session's owner has moved on.
	authOpts := []auth.Option{auth.WithAutoRefresh(false)}
	if onRefresh != nil {
		authOpts = append(authOpts, auth.WithTokenRefreshCallback(func(t *auth.TokenResponse) {
			data, err := json.Marshal(t) // #nosec G117 -- the token is intentionally serialized for storage in a K8s Secret
			if err != nil {
				return
			}
			onRefresh(data)
		}))
	}
	mgr := c.AuthManager(authOpts...)
	mgr.LoadToken(&tok)

	var clientOpts []scp.ClientOption
	if c.APIBaseURL != "" {
		clientOpts = append(clientOpts, scp.WithBaseURL(c.APIBaseURL))
	}
	client, err := scp.NewClient(mgr, clientOpts...)
	if err != nil {
		mgr.Close()
		return nil, fmt.Errorf("creating SCP client: %w", err)
	}
	return &Session{API: client, UserID: userID, close: client.Close}, nil
}

// IsRateLimited reports whether err is an HTTP 429 from the SCP API (failover
// routing is limited to 10 requests per 5 minutes). The generated client
// surfaces status codes only in error strings.
func IsRateLimited(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unexpected status code: 429")
}

// jwtUserID extracts the numeric SCP user ID from the "id" claim of a JWT
// access token. The standard "sub" claim holds the Keycloak account UUID, not
// the numeric ID used in API paths. Claims are decoded without signature
// validation: the token is only used to address our own API requests.
func jwtUserID(accessToken string) (int32, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return 0, errors.New("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, fmt.Errorf("decoding JWT payload: %w", err)
	}
	var claims struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0, fmt.Errorf("parsing JWT claims: %w", err)
	}
	if claims.ID == "" {
		return 0, errors.New("JWT has no id claim")
	}
	id, err := strconv.ParseInt(claims.ID, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing user ID %q: %w", claims.ID, err)
	}
	return int32(id), nil
}
