package netcup

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/digilolnet/go-netcup-scp/pkg/scp"
)

// Route is a failover IP from the CRD spec, parsed for matching against the
// account's failover IP entries.
type Route struct {
	cidr   string
	addr   net.IP
	prefix int32
	isIPv6 bool
}

// ParseRoutes parses spec CIDRs (e.g. 198.51.100.1/32, 2001:db8::/64) into
// Routes. Host bits are kept: 198.51.100.7/24 targets 198.51.100.7.
func ParseRoutes(cidrs []string) ([]Route, error) {
	routes := make([]Route, 0, len(cidrs))
	for _, cidr := range cidrs {
		addr, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}
		ones, _ := network.Mask.Size()
		routes = append(routes, Route{
			cidr:   cidr,
			addr:   addr,
			prefix: int32(ones), // #nosec G115 -- CIDR mask size is 0-128
			isIPv6: addr.To4() == nil,
		})
	}
	return routes, nil
}

// ServerIDsByName returns the numeric ID of every server in the account,
// keyed by server name. Callers use it both to resolve a node's server-name
// annotation and to know which nodes this account can route to at all.
func (s *Session) ServerIDsByName(ctx context.Context) (map[string]int32, error) {
	callCtx, cancel := s.callContext(ctx)
	defer cancel()
	servers, err := s.API.ListServers(callCtx, nil)
	if err != nil {
		return nil, fmt.Errorf("listing servers: %w", err)
	}
	ids := make(map[string]int32, len(servers))
	for _, srv := range servers {
		if srv.Name != nil && srv.Id != nil {
			ids[*srv.Name] = *srv.Id
		}
	}
	return ids, nil
}

// EnsureRouted routes every Route not already pointing at serverID, waiting
// for each async routing task to finish. Routing state comes from the API's
// failover IP listings, so a retry after a partial failure, rate limit, or
// crash skips the routes that already succeeded.
func (s *Session) EnsureRouted(ctx context.Context, routes []Route, serverID int32) error {
	v4, v6, err := s.listFailoverIPs(ctx, routes)
	if err != nil {
		return err
	}

	for _, rt := range routes {
		failoverID, routedTo, err := findFailoverIP(rt, v4, v6)
		if err != nil {
			return err
		}
		if routedTo != nil && *routedTo == serverID {
			continue
		}

		task, err := s.route(ctx, rt, failoverID, serverID)
		if err != nil {
			return fmt.Errorf("routing %s: %w", rt.cidr, err)
		}
		if err := s.waitTask(ctx, task); err != nil {
			return fmt.Errorf("routing %s: %w", rt.cidr, err)
		}
	}
	return nil
}

// Misrouted returns the CIDRs of the routes netcup does not currently route to
// serverID. It only reads, so it does not count against the routing rate
// limit.
func (s *Session) Misrouted(ctx context.Context, routes []Route, serverID int32) ([]string, error) {
	v4, v6, err := s.listFailoverIPs(ctx, routes)
	if err != nil {
		return nil, err
	}
	var misrouted []string
	for _, rt := range routes {
		_, routedTo, err := findFailoverIP(rt, v4, v6)
		if err != nil {
			return nil, err
		}
		if routedTo == nil || *routedTo != serverID {
			misrouted = append(misrouted, rt.cidr)
		}
	}
	return misrouted, nil
}

// callContext derives the context for one SCP API call, bounded by
// CallTimeout.
func (s *Session) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, cmp.Or(s.CallTimeout, defaultCallTimeout))
}

// listFailoverIPs fetches the account's failover IPs, skipping the address
// family no route needs.
func (s *Session) listFailoverIPs(ctx context.Context, routes []Route) ([]scp.FailoverIPv4, []scp.FailoverIPv6, error) {
	var v4 []scp.FailoverIPv4
	var v6 []scp.FailoverIPv6
	if slices.ContainsFunc(routes, func(r Route) bool { return !r.isIPv6 }) {
		callCtx, cancel := s.callContext(ctx)
		defer cancel()
		var err error
		if v4, err = s.API.ListFailoverIPv4(callCtx, s.UserID, nil); err != nil {
			return nil, nil, fmt.Errorf("listing failover IPv4 addresses: %w", err)
		}
	}
	if slices.ContainsFunc(routes, func(r Route) bool { return r.isIPv6 }) {
		callCtx, cancel := s.callContext(ctx)
		defer cancel()
		var err error
		if v6, err = s.API.ListFailoverIPv6(callCtx, s.UserID, nil); err != nil {
			return nil, nil, fmt.Errorf("listing failover IPv6 prefixes: %w", err)
		}
	}
	return v4, v6, nil
}

// route issues the routing request for one failover IP and returns its async
// task.
func (s *Session) route(ctx context.Context, rt Route, failoverID, serverID int32) (*scp.TaskInfo, error) {
	ctx, cancel := s.callContext(ctx)
	defer cancel()
	if rt.isIPv6 {
		return s.API.RouteFailoverIPv6(ctx, s.UserID, failoverID, serverID)
	}
	return s.API.RouteFailoverIPv4(ctx, s.UserID, failoverID, serverID)
}

// getTask fetches the current state of an async task.
func (s *Session) getTask(ctx context.Context, uuid string) (*scp.TaskInfo, error) {
	ctx, cancel := s.callContext(ctx)
	defer cancel()
	return s.API.GetTask(ctx, uuid)
}

// findFailoverIP matches a Route against the account's failover IPs and
// returns the failover ID plus the server it is currently routed to (nil if
// unrouted).
func findFailoverIP(rt Route, v4 []scp.FailoverIPv4, v6 []scp.FailoverIPv6) (int32, *int32, error) {
	if rt.isIPv6 {
		for _, entry := range v6 {
			if entry.Id == nil || entry.NetworkPrefix == nil {
				continue
			}
			if prefix := net.ParseIP(*entry.NetworkPrefix); prefix == nil || !prefix.Equal(rt.addr) {
				continue
			}
			if entry.NetworkPrefixLength != nil && *entry.NetworkPrefixLength != rt.prefix {
				continue
			}
			var routedTo *int32
			if entry.Server != nil {
				routedTo = entry.Server.Id
			}
			return *entry.Id, routedTo, nil
		}
	} else {
		for _, entry := range v4 {
			if entry.Id == nil || entry.Ip == nil {
				continue
			}
			if addr := net.ParseIP(*entry.Ip); addr == nil || !addr.Equal(rt.addr) {
				continue
			}
			var routedTo *int32
			if entry.Server != nil {
				routedTo = entry.Server.Id
			}
			return *entry.Id, routedTo, nil
		}
	}
	return 0, nil, fmt.Errorf("failover IP %s not found in the netcup account", rt.cidr)
}

// waitTask polls the async routing task until it finishes. A task that ends
// in ERROR or CANCELED is a routing failure.
func (s *Session) waitTask(ctx context.Context, task *scp.TaskInfo) error {
	if task == nil || task.Uuid == nil {
		return nil
	}
	timeout := cmp.Or(s.TaskTimeout, defaultTaskTimeout)
	interval := cmp.Or(s.TaskPollInterval, defaultTaskPollInterval)
	deadline := time.Now().Add(timeout)
	for {
		if task.State != nil {
			switch *task.State {
			case scp.TaskStateFINISHED:
				return nil
			case scp.TaskStateERROR, scp.TaskStateCANCELED:
				return fmt.Errorf("task %s ended in state %s: %s", *task.Uuid, *task.State, taskMessage(task))
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s still running after %s", *task.Uuid, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		next, err := s.getTask(ctx, *task.Uuid)
		if err != nil {
			return fmt.Errorf("polling task %s: %w", *task.Uuid, err)
		}
		// Keep the UUID from the original 202 in case the poll response
		// omits it; the loop needs it for the next GetTask and for errors.
		next.Uuid = task.Uuid
		task = next
	}
}

func taskMessage(task *scp.TaskInfo) string {
	if task.ResponseError != nil && task.ResponseError.Message != nil {
		return *task.ResponseError.Message
	}
	if task.Message != nil {
		return *task.Message
	}
	return "no error message"
}
