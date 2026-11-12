package netcup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digilolnet/go-netcup-scp/pkg/scp"
)

// unmarshalInto merges a JSON fragment into v; the only way to populate
// fields of the library's internal generated types from outside its module.
func unmarshalInto(t *testing.T, v any, format string, args ...any) {
	t.Helper()
	if err := json.Unmarshal(fmt.Appendf(nil, format, args...), v); err != nil {
		t.Fatalf("building fixture: %v", err)
	}
}

func serverFixture(t *testing.T, id int32, name string) scp.ServerListMinimal {
	t.Helper()
	var s scp.ServerListMinimal
	unmarshalInto(t, &s, `{"id":%d,"name":%q}`, id, name)
	return s
}

func fv4(t *testing.T, id int32, ip string, routedTo *int32) scp.FailoverIPv4 {
	t.Helper()
	var e scp.FailoverIPv4
	unmarshalInto(t, &e, `{"id":%d,"ip":%q}`, id, ip)
	if routedTo != nil {
		unmarshalInto(t, &e, `{"server":{"id":%d}}`, *routedTo)
	}
	return e
}

func fv6(t *testing.T, id int32, prefix string, length int32, routedTo *int32) scp.FailoverIPv6 {
	t.Helper()
	var e scp.FailoverIPv6
	unmarshalInto(t, &e, `{"id":%d,"networkPrefix":%q,"networkPrefixLength":%d}`, id, prefix, length)
	if routedTo != nil {
		unmarshalInto(t, &e, `{"server":{"id":%d}}`, *routedTo)
	}
	return e
}

// stubAPI implements API for routing tests.
type stubAPI struct {
	servers        []scp.ServerListMinimal
	v4             []scp.FailoverIPv4
	v6             []scp.FailoverIPv6
	listServersErr error
	listV4Err      error
	listV6Err      error
	routeErr       error
	getTaskErr     error
	hang           bool          // ListServers blocks until its context ends
	taskJSON       string        // extra JSON merged into the task returned by route calls
	taskState      scp.TaskState // task state returned by route calls; defaults to FINISHED
	pendingPolls   int           // GetTask calls that report RUNNING before FINISHED

	routeCalls   []string // "v4:<failoverID>-><serverID>"
	getTaskCalls int
}

func (m *stubAPI) ListServers(ctx context.Context, _ *scp.ListServersOptions) ([]scp.ServerListMinimal, error) {
	if m.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return m.servers, m.listServersErr
}

func (m *stubAPI) ListFailoverIPv4(context.Context, int32, *scp.ListFailoverIPsOptions) ([]scp.FailoverIPv4, error) {
	return m.v4, m.listV4Err
}

func (m *stubAPI) ListFailoverIPv6(context.Context, int32, *scp.ListFailoverIPsOptions) ([]scp.FailoverIPv6, error) {
	return m.v6, m.listV6Err
}

func (m *stubAPI) route(family string, failoverID, serverID int32) (*scp.TaskInfo, error) {
	m.routeCalls = append(m.routeCalls, fmt.Sprintf("%s:%d->%d", family, failoverID, serverID))
	if m.routeErr != nil {
		return nil, m.routeErr
	}
	state := m.taskState
	if state == "" {
		state = scp.TaskStateFINISHED
	}
	var task scp.TaskInfo
	task.Uuid = new("task-1")
	task.State = &state
	if m.taskJSON != "" {
		if err := json.Unmarshal([]byte(m.taskJSON), &task); err != nil {
			return nil, fmt.Errorf("building task fixture: %w", err)
		}
	}
	return &task, nil
}

func (m *stubAPI) RouteFailoverIPv4(_ context.Context, _, failoverID, serverID int32) (*scp.TaskInfo, error) {
	return m.route("v4", failoverID, serverID)
}

func (m *stubAPI) RouteFailoverIPv6(_ context.Context, _, failoverID, serverID int32) (*scp.TaskInfo, error) {
	return m.route("v6", failoverID, serverID)
}

func (m *stubAPI) GetTask(_ context.Context, uuid string) (*scp.TaskInfo, error) {
	m.getTaskCalls++
	if m.getTaskErr != nil {
		return nil, m.getTaskErr
	}
	state := scp.TaskStateFINISHED
	if m.getTaskCalls <= m.pendingPolls {
		state = "RUNNING"
	}
	var task scp.TaskInfo
	task.Uuid = &uuid
	task.State = &state
	return &task, nil
}

func testSession(api API) *Session {
	return &Session{
		API:              api,
		UserID:           42,
		TaskPollInterval: time.Millisecond,
		TaskTimeout:      time.Second,
	}
}

func TestParseRoutes(t *testing.T) {
	tests := []struct {
		name       string
		cidrs      []string
		wantAddr   string
		wantPrefix int32
		wantIPv6   bool
		wantErr    bool
	}{
		{name: "ipv4 host", cidrs: []string{"198.51.100.1/32"}, wantAddr: "198.51.100.1", wantPrefix: 32},
		{name: "ipv4 keeps host bits", cidrs: []string{"198.51.100.7/24"}, wantAddr: "198.51.100.7", wantPrefix: 24},
		{name: "ipv6 subnet", cidrs: []string{"2001:db8::/64"}, wantAddr: "2001:db8::", wantPrefix: 64, wantIPv6: true},
		{name: "missing prefix", cidrs: []string{"198.51.100.1"}, wantErr: true},
		{name: "garbage", cidrs: []string{"not-a-cidr"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes, err := ParseRoutes(tt.cidrs)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			got := routes[0]
			if got.addr.String() != tt.wantAddr || got.prefix != tt.wantPrefix || got.isIPv6 != tt.wantIPv6 {
				t.Errorf("ParseRoutes = %s/%d (ipv6=%v), want %s/%d (ipv6=%v)",
					got.addr, got.prefix, got.isIPv6, tt.wantAddr, tt.wantPrefix, tt.wantIPv6)
			}
		})
	}
}

func TestSession_ServerIDsByName(t *testing.T) {
	t.Run("maps all servers", func(t *testing.T) {
		api := &stubAPI{servers: []scp.ServerListMinimal{
			serverFixture(t, 101, "srv-a"),
			serverFixture(t, 102, "srv-b"),
		}}
		ids, err := testSession(api).ServerIDsByName(t.Context())
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if len(ids) != 2 || ids["srv-a"] != 101 || ids["srv-b"] != 102 {
			t.Errorf("ServerIDsByName = %v, want srv-a:101 srv-b:102", ids)
		}
	})

	t.Run("list error", func(t *testing.T) {
		api := &stubAPI{listServersErr: errors.New("boom")}
		if _, err := testSession(api).ServerIDsByName(t.Context()); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("error = %v, want wrapped list error", err)
		}
	})
}

func mustParseRoutes(t *testing.T, cidrs ...string) []Route {
	t.Helper()
	routes, err := ParseRoutes(cidrs)
	if err != nil {
		t.Fatalf("parsing routes: %v", err)
	}
	return routes
}

func TestSession_EnsureRouted(t *testing.T) {
	t.Run("routes all unrouted IPs", func(t *testing.T) {
		api := &stubAPI{
			v4: []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)},
			v6: []scp.FailoverIPv6{fv6(t, 21, "2001:db8::", 64, nil)},
		}
		routes := mustParseRoutes(t, "198.51.100.1/32", "2001:db8::/64")

		if err := testSession(api).EnsureRouted(t.Context(), routes, 101); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		want := []string{"v4:11->101", "v6:21->101"}
		if !slices.Equal(api.routeCalls, want) {
			t.Errorf("route calls = %v, want %v", api.routeCalls, want)
		}
	})

	t.Run("skips IPs already on the target server", func(t *testing.T) {
		api := &stubAPI{
			v4: []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", new(int32(101)))},
			v6: []scp.FailoverIPv6{fv6(t, 21, "2001:db8::", 64, new(int32(999)))},
		}
		routes := mustParseRoutes(t, "198.51.100.1/32", "2001:db8::/64")

		if err := testSession(api).EnsureRouted(t.Context(), routes, 101); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if !slices.Equal(api.routeCalls, []string{"v6:21->101"}) {
			t.Errorf("route calls = %v, want only the v6 re-route", api.routeCalls)
		}
	})

	t.Run("unknown failover IP", func(t *testing.T) {
		api := &stubAPI{v4: []scp.FailoverIPv4{fv4(t, 11, "203.0.113.9", nil)}}
		err := testSession(api).EnsureRouted(t.Context(), mustParseRoutes(t, "198.51.100.1/32"), 101)
		if err == nil || !strings.Contains(err.Error(), "not found in the netcup account") {
			t.Fatalf("error = %v, want unknown-IP error", err)
		}
	})

	t.Run("route error is wrapped with the CIDR", func(t *testing.T) {
		api := &stubAPI{
			v4:       []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)},
			routeErr: errors.New("unexpected status code: 429"),
		}
		err := testSession(api).EnsureRouted(t.Context(), mustParseRoutes(t, "198.51.100.1/32"), 101)
		if err == nil || !strings.Contains(err.Error(), "routing 198.51.100.1/32") || !IsRateLimited(err) {
			t.Fatalf("error = %v, want CIDR-wrapped rate-limit error", err)
		}
	})

	t.Run("task ending in ERROR fails", func(t *testing.T) {
		api := &stubAPI{
			v4:        []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)},
			taskState: scp.TaskStateERROR,
		}
		err := testSession(api).EnsureRouted(t.Context(), mustParseRoutes(t, "198.51.100.1/32"), 101)
		if err == nil || !strings.Contains(err.Error(), "ended in state ERROR") {
			t.Fatalf("error = %v, want task-error failure", err)
		}
	})

	t.Run("polls a running task until finished", func(t *testing.T) {
		api := &stubAPI{
			v4:           []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)},
			taskState:    "RUNNING",
			pendingPolls: 2,
		}
		if err := testSession(api).EnsureRouted(t.Context(), mustParseRoutes(t, "198.51.100.1/32"), 101); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if api.getTaskCalls != 3 {
			t.Errorf("expected exactly 3 polls (2 RUNNING, then FINISHED), got %d", api.getTaskCalls)
		}
	})

	t.Run("running task times out", func(t *testing.T) {
		api := &stubAPI{
			v4:           []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)},
			taskState:    "RUNNING",
			pendingPolls: 1 << 30, // never finishes
		}
		session := testSession(api)
		session.TaskTimeout = 5 * time.Millisecond
		err := session.EnsureRouted(t.Context(), mustParseRoutes(t, "198.51.100.1/32"), 101)
		if err == nil || !strings.Contains(err.Error(), "still running after") {
			t.Fatalf("error = %v, want timeout error", err)
		}
	})
}

func TestSession_EnsureRouted_Failures(t *testing.T) {
	v4 := func(t *testing.T) []scp.FailoverIPv4 {
		t.Helper()
		return []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)}
	}

	tests := []struct {
		name    string
		api     func(t *testing.T) *stubAPI
		cidr    string
		wantErr string
	}{
		{
			name:    "listing IPv4 addresses fails",
			api:     func(*testing.T) *stubAPI { return &stubAPI{listV4Err: errors.New("boom")} },
			cidr:    "198.51.100.1/32",
			wantErr: "listing failover IPv4 addresses: boom",
		},
		{
			name:    "listing IPv6 prefixes fails",
			api:     func(*testing.T) *stubAPI { return &stubAPI{listV6Err: errors.New("boom")} },
			cidr:    "2001:db8::/64",
			wantErr: "listing failover IPv6 prefixes: boom",
		},
		{
			name: "canceled task fails",
			api: func(t *testing.T) *stubAPI {
				t.Helper()
				return &stubAPI{v4: v4(t), taskState: scp.TaskStateCANCELED}
			},
			cidr:    "198.51.100.1/32",
			wantErr: "ended in state CANCELED",
		},
		{
			name: "task error message comes from the response error",
			api: func(t *testing.T) *stubAPI {
				t.Helper()
				return &stubAPI{
					v4:        v4(t),
					taskState: scp.TaskStateERROR,
					taskJSON:  `{"message":"generic","responseError":{"message":"server is locked"}}`,
				}
			},
			cidr:    "198.51.100.1/32",
			wantErr: "ended in state ERROR: server is locked",
		},
		{
			name: "task error message falls back to the task message",
			api: func(t *testing.T) *stubAPI {
				t.Helper()
				return &stubAPI{v4: v4(t), taskState: scp.TaskStateERROR, taskJSON: `{"message":"generic"}`}
			},
			cidr:    "198.51.100.1/32",
			wantErr: "ended in state ERROR: generic",
		},
		{
			name: "poll error is wrapped with the task UUID",
			api: func(t *testing.T) *stubAPI {
				t.Helper()
				return &stubAPI{v4: v4(t), taskState: "RUNNING", getTaskErr: errors.New("boom")}
			},
			cidr:    "198.51.100.1/32",
			wantErr: "polling task task-1: boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := testSession(tt.api(t)).EnsureRouted(t.Context(), mustParseRoutes(t, tt.cidr), 101)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}

	t.Run("context canceled while polling", func(t *testing.T) {
		api := &stubAPI{v4: v4(t), taskState: "RUNNING", pendingPolls: 1 << 30}
		session := testSession(api)
		session.TaskPollInterval = time.Hour // the cancellation must win the select
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := session.EnsureRouted(ctx, mustParseRoutes(t, "198.51.100.1/32"), 101)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

// The zero-value Session must behave like one built by Connect: production
// defaults for the call timeout, poll interval and task timeout. synctest's
// fake clock makes the minutes-long waits instant.
func TestSession_ZeroValueDefaults(t *testing.T) {
	t.Run("a stalled API call is cut off by the call timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			session := &Session{API: &stubAPI{hang: true}, UserID: 42}
			start := time.Now()
			_, err := session.ServerIDsByName(t.Context())
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want context.DeadlineExceeded", err)
			}
			if got := time.Since(start); got != defaultCallTimeout {
				t.Errorf("call was cut off after %s, want %s", got, defaultCallTimeout)
			}
		})
	})

	t.Run("a task that never finishes times out after the default", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			api := &stubAPI{
				v4:           []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)},
				taskState:    "RUNNING",
				pendingPolls: 1 << 30,
			}
			session := &Session{API: api, UserID: 42}
			err := session.EnsureRouted(t.Context(), mustParseRoutes(t, "198.51.100.1/32"), 101)
			if err == nil || !strings.Contains(err.Error(), "still running after "+defaultTaskTimeout.String()) {
				t.Fatalf("error = %v, want the default task timeout", err)
			}
			if want := int(defaultTaskTimeout / defaultTaskPollInterval); api.getTaskCalls < want {
				t.Errorf("task polled %d times, want at least %d (default interval)", api.getTaskCalls, want)
			}
		})
	})
}

func TestFindFailoverIP(t *testing.T) {
	var noID scp.FailoverIPv4
	unmarshalInto(t, &noID, `{"ip":"198.51.100.1"}`)
	var noPrefix scp.FailoverIPv6
	unmarshalInto(t, &noPrefix, `{"id":20}`)

	v4 := []scp.FailoverIPv4{noID, fv4(t, 11, "198.51.100.1", new(int32(101))), fv4(t, 12, "198.51.100.2", nil)}
	v6 := []scp.FailoverIPv6{noPrefix, fv6(t, 21, "2001:db8::", 48, nil), fv6(t, 22, "2001:db8:1::", 64, new(int32(102)))}

	tests := []struct {
		name         string
		cidr         string
		wantID       int32
		wantRoutedTo *int32
		wantErr      bool
	}{
		{name: "v4 routed", cidr: "198.51.100.1/32", wantID: 11, wantRoutedTo: new(int32(101))},
		{name: "v4 unrouted", cidr: "198.51.100.2/32", wantID: 12},
		{name: "v4 unknown", cidr: "198.51.100.3/32", wantErr: true},
		{name: "v6 routed", cidr: "2001:db8:1::/64", wantID: 22, wantRoutedTo: new(int32(102))},
		{name: "v6 written differently", cidr: "2001:0db8:0001::/64", wantID: 22, wantRoutedTo: new(int32(102))},
		{name: "v6 same prefix but different length", cidr: "2001:db8::/64", wantErr: true},
		{name: "v6 unknown", cidr: "2001:db8:2::/64", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, routedTo, err := findFailoverIP(mustParseRoutes(t, tt.cidr)[0], v4, v6)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got failover ID %d", id)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if id != tt.wantID {
				t.Errorf("failover ID = %d, want %d", id, tt.wantID)
			}
			switch {
			case (routedTo == nil) != (tt.wantRoutedTo == nil):
				t.Errorf("routedTo = %v, want %v", routedTo, tt.wantRoutedTo)
			case routedTo != nil && *routedTo != *tt.wantRoutedTo:
				t.Errorf("routedTo = %d, want %d", *routedTo, *tt.wantRoutedTo)
			}
		})
	}
}

func TestSession_Misrouted(t *testing.T) {
	target, other := int32(101), int32(102)
	api := &stubAPI{
		v4: []scp.FailoverIPv4{
			fv4(t, 1, "198.51.100.1", &target),
			fv4(t, 2, "198.51.100.2", &other),
		},
		v6: []scp.FailoverIPv6{fv6(t, 3, "2001:db8::", 64, nil)},
	}
	routes := mustParseRoutes(t, "198.51.100.1/32", "198.51.100.2/32", "2001:db8::/64")

	got, err := testSession(api).Misrouted(t.Context(), routes, target)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if want := []string{"198.51.100.2/32", "2001:db8::/64"}; !slices.Equal(got, want) {
		t.Errorf("Misrouted = %v, want %v", got, want)
	}
	if len(api.routeCalls) != 0 {
		t.Errorf("Misrouted must only read, got route calls: %v", api.routeCalls)
	}

	if _, err := testSession(api).Misrouted(t.Context(), mustParseRoutes(t, "203.0.113.9/32"), target); err == nil {
		t.Error("expected an error for a failover IP missing from the account")
	}
}
