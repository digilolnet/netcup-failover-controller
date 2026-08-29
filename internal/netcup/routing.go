package netcup

import (
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
	servers, err := s.API.ListServers(ctx, nil)
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
	var v4 []scp.FailoverIPv4
	var v6 []scp.FailoverIPv6
	var err error
	if slices.ContainsFunc(routes, func(r Route) bool { return !r.isIPv6 }) {
		if v4, err = s.API.ListFailoverIPv4(ctx, s.UserID, nil); err != nil {
			return fmt.Errorf("listing failover IPv4 addresses: %w", err)
		}
	}
	if slices.ContainsFunc(routes, func(r Route) bool { return r.isIPv6 }) {
		if v6, err = s.API.ListFailoverIPv6(ctx, s.UserID, nil); err != nil {
			return fmt.Errorf("listing failover IPv6 prefixes: %w", err)
		}
	}

	for _, rt := range routes {
		failoverID, routedTo, err := findFailoverIP(rt, v4, v6)
		if err != nil {
			return err
		}
		if routedTo != nil && *routedTo == serverID {
			continue
		}

		var task *scp.TaskInfo
		if rt.isIPv6 {
			task, err = s.API.RouteFailoverIPv6(ctx, s.UserID, failoverID, serverID)
		} else {
			task, err = s.API.RouteFailoverIPv4(ctx, s.UserID, failoverID, serverID)
		}
		if err != nil {
			return fmt.Errorf("routing %s: %w", rt.cidr, err)
		}
		if err := s.waitTask(ctx, task); err != nil {
			return fmt.Errorf("routing %s: %w", rt.cidr, err)
		}
	}
	return nil
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
	deadline := time.Now().Add(s.TaskTimeout)
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
			return fmt.Errorf("task %s still running after %s", *task.Uuid, s.TaskTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.TaskPollInterval):
		}
		next, err := s.API.GetTask(ctx, *task.Uuid)
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
