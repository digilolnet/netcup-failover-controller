package netcup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digilolnet/go-netcup-scp/pkg/scp"
)

func ptr[T any](v T) *T { return &v }

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
	routeErr       error
	taskState      scp.TaskState // task state returned by route calls; defaults to FINISHED
	pendingPolls   int           // GetTask calls that report RUNNING before FINISHED

	routeCalls   []string // "v4:<failoverID>-><serverID>"
	getTaskCalls int
}

func (m *stubAPI) ListServers(context.Context, *scp.ListServersOptions) ([]scp.ServerListMinimal, error) {
	return m.servers, m.listServersErr
}

func (m *stubAPI) ListFailoverIPv4(context.Context, int32, *scp.ListFailoverIPsOptions) ([]scp.FailoverIPv4, error) {
	return m.v4, nil
}

func (m *stubAPI) ListFailoverIPv6(context.Context, int32, *scp.ListFailoverIPsOptions) ([]scp.FailoverIPv6, error) {
	return m.v6, nil
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
	task.Uuid = ptr("task-1")
	task.State = &state
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
		if fmt.Sprint(api.routeCalls) != fmt.Sprint(want) {
			t.Errorf("route calls = %v, want %v", api.routeCalls, want)
		}
	})

	t.Run("skips IPs already on the target server", func(t *testing.T) {
		api := &stubAPI{
			v4: []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", ptr(int32(101)))},
			v6: []scp.FailoverIPv6{fv6(t, 21, "2001:db8::", 64, ptr(int32(999)))},
		}
		routes := mustParseRoutes(t, "198.51.100.1/32", "2001:db8::/64")

		if err := testSession(api).EnsureRouted(t.Context(), routes, 101); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if fmt.Sprint(api.routeCalls) != "[v6:21->101]" {
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
		if api.getTaskCalls < 3 {
			t.Errorf("expected the task to be polled until FINISHED, got %d polls", api.getTaskCalls)
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
