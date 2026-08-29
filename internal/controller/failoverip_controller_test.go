package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digilolnet/go-netcup-scp/pkg/scp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	netcupv1alpha1 "github.com/digilolnet/netcup-failover-controller/api/v1alpha1"
	"github.com/digilolnet/netcup-failover-controller/internal/netcup"
)

func ptr[T any](v T) *T { return &v }

type routeCall struct {
	family     string // "v4" or "v6"
	failoverID int32
	serverID   int32
}

// mockAPI implements netcup.API. Successful route calls update the entry's
// routed server, mirroring the real API's authoritative routing state.
type mockAPI struct {
	t              *testing.T
	servers        []scp.ServerListMinimal
	v4             []scp.FailoverIPv4
	v6             []scp.FailoverIPv6
	listServersErr error
	routeErr       func(call routeCall) error
	taskState      scp.TaskState // task state returned by route calls; defaults to FINISHED

	routeCalls []routeCall
}

// unmarshalInto merges a JSON fragment into v; the only way to populate
// fields of the library's internal generated types from outside its module.
func unmarshalInto(t *testing.T, v any, format string, args ...any) {
	t.Helper()
	if err := json.Unmarshal(fmt.Appendf(nil, format, args...), v); err != nil {
		t.Fatalf("building fixture: %v", err)
	}
}

func server(t *testing.T, id int32, name string) scp.ServerListMinimal {
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

func (m *mockAPI) ListServers(_ context.Context, _ *scp.ListServersOptions) ([]scp.ServerListMinimal, error) {
	return m.servers, m.listServersErr
}

func (m *mockAPI) ListFailoverIPv4(_ context.Context, _ int32, _ *scp.ListFailoverIPsOptions) ([]scp.FailoverIPv4, error) {
	return m.v4, nil
}

func (m *mockAPI) ListFailoverIPv6(_ context.Context, _ int32, _ *scp.ListFailoverIPsOptions) ([]scp.FailoverIPv6, error) {
	return m.v6, nil
}

func (m *mockAPI) route(call routeCall) (*scp.TaskInfo, error) {
	t := m.t
	m.routeCalls = append(m.routeCalls, call)
	if m.routeErr != nil {
		if err := m.routeErr(call); err != nil {
			return nil, err
		}
	}
	// Mirror the routing into the listed state.
	if call.family == "v4" {
		for i := range m.v4 {
			if m.v4[i].Id != nil && *m.v4[i].Id == call.failoverID {
				unmarshalInto(t, &m.v4[i], `{"server":{"id":%d}}`, call.serverID)
			}
		}
	} else {
		for i := range m.v6 {
			if m.v6[i].Id != nil && *m.v6[i].Id == call.failoverID {
				unmarshalInto(t, &m.v6[i], `{"server":{"id":%d}}`, call.serverID)
			}
		}
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

func (m *mockAPI) RouteFailoverIPv4(_ context.Context, _, failoverID, serverID int32) (*scp.TaskInfo, error) {
	return m.route(routeCall{family: "v4", failoverID: failoverID, serverID: serverID})
}

func (m *mockAPI) RouteFailoverIPv6(_ context.Context, _, failoverID, serverID int32) (*scp.TaskInfo, error) {
	return m.route(routeCall{family: "v6", failoverID: failoverID, serverID: serverID})
}

func (m *mockAPI) GetTask(_ context.Context, uuid string) (*scp.TaskInfo, error) {
	state := scp.TaskStateFINISHED
	var task scp.TaskInfo
	task.Uuid = &uuid
	task.State = &state
	return &task, nil
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding corev1 to scheme: %v", err)
	}
	if err := netcupv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding netcup v1alpha1 to scheme: %v", err)
	}
	return scheme
}

func readyNode(name, serverName string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if serverName != "" {
		node.Annotations = map[string]string{annotationServerName: serverName}
	}
	node.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
	}
	return node
}

func notReadyNode(name string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	node.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}
	return node
}

func testSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "netcup-credentials", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte(`{"access_token":"a","refresh_token":"r"}`)},
	}
}

func testFIP(name string, ips ...string) *netcupv1alpha1.NetcupFailoverIP {
	return &netcupv1alpha1.NetcupFailoverIP{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec: netcupv1alpha1.NetcupFailoverIPSpec{
			IPs:               ips,
			CredentialsSecret: corev1.LocalObjectReference{Name: "netcup-credentials"},
		},
	}
}

func routedFIP(name string, ips ...string) *netcupv1alpha1.NetcupFailoverIP {
	fip := testFIP(name, ips...)
	fip.Status.CurrentNode = "node-a"
	fip.Status.Conditions = []metav1.Condition{{
		Type:               netcupv1alpha1.ConditionRouted,
		Status:             metav1.ConditionTrue,
		Reason:             netcupv1alpha1.ReasonNodeSelected,
		ObservedGeneration: 1,
		LastTransitionTime: metav1.Now(),
	}}
	return fip
}

func newReconciler(t *testing.T, api *mockAPI, objs ...client.Object) *FailoverIPReconciler {
	t.Helper()
	api.t = t
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&netcupv1alpha1.NetcupFailoverIP{}).
		Build()
	return &FailoverIPReconciler{
		Client: cl,
		Connect: func(_ []byte, _ func([]byte)) (*netcup.Session, error) {
			return &netcup.Session{
				API:              api,
				UserID:           42,
				TaskPollInterval: time.Millisecond,
				TaskTimeout:      time.Second,
			}, nil
		},
		CredentialsNamespace: "default", // test secrets live in "default"
	}
}

func reconcileOnce(t *testing.T, r *FailoverIPReconciler, name string) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
}

func getFIP(t *testing.T, r *FailoverIPReconciler, name string) *netcupv1alpha1.NetcupFailoverIP {
	t.Helper()
	var fip netcupv1alpha1.NetcupFailoverIP
	if err := r.Get(t.Context(), types.NamespacedName{Name: name}, &fip); err != nil {
		t.Fatalf("getting NetcupFailoverIP %s: %v", name, err)
	}
	return &fip
}

func routedCondition(t *testing.T, r *FailoverIPReconciler, name string) *metav1.Condition {
	t.Helper()
	fip := getFIP(t, r, name)
	cond := meta.FindStatusCondition(fip.Status.Conditions, netcupv1alpha1.ConditionRouted)
	if cond == nil {
		t.Fatalf("Routed condition not set on %s", name)
	}
	return cond
}

func TestReconcile_RoutesAllIPs(t *testing.T) {
	api := &mockAPI{
		servers: []scp.ServerListMinimal{server(t, 101, "srv-a")},
		v4:      []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)},
		v6:      []scp.FailoverIPv6{fv6(t, 21, "2001:db8::", 64, nil)},
	}
	r := newReconciler(t, api,
		testFIP("group-a", "198.51.100.1/32", "2001:db8::/64"),
		readyNode("node-a", "srv-a"),
		testSecret(),
	)

	res, err := reconcileOnce(t, r, "group-a")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("expected empty result, got: %+v", res)
	}

	want := []routeCall{
		{family: "v4", failoverID: 11, serverID: 101},
		{family: "v6", failoverID: 21, serverID: 101},
	}
	if len(api.routeCalls) != len(want) {
		t.Fatalf("expected %d route calls, got: %+v", len(want), api.routeCalls)
	}
	for i, call := range api.routeCalls {
		if call != want[i] {
			t.Errorf("route call %d = %+v, want %+v", i, call, want[i])
		}
	}

	fip := getFIP(t, r, "group-a")
	if fip.Status.CurrentNode != "node-a" {
		t.Errorf("currentNode = %q, want node-a", fip.Status.CurrentNode)
	}
	if cond := routedCondition(t, r, "group-a"); cond.Status != metav1.ConditionTrue {
		t.Errorf("Routed condition = %s (%s), want True", cond.Status, cond.Reason)
	}
}

func TestReconcile_SkipsAlreadyRoutedIPs(t *testing.T) {
	api := &mockAPI{
		servers: []scp.ServerListMinimal{server(t, 101, "srv-a")},
		v4:      []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", ptr(int32(101)))}, // already on target
		v6:      []scp.FailoverIPv6{fv6(t, 21, "2001:db8::", 64, nil)},
	}
	r := newReconciler(t, api,
		testFIP("group-a", "198.51.100.1/32", "2001:db8::/64"),
		readyNode("node-a", "srv-a"),
		testSecret(),
	)

	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(api.routeCalls) != 1 || api.routeCalls[0].family != "v6" {
		t.Fatalf("expected only the v6 prefix to be routed, got: %+v", api.routeCalls)
	}
}

func TestReconcile_NoOpWhenRoutedAndHealthy(t *testing.T) {
	// The credentials secret is deliberately absent: a no-op reconcile must
	// return before reading credentials, so its absence must not error.
	api := &mockAPI{}
	r := newReconciler(t, api, routedFIP("group-a", "198.51.100.1/32"), readyNode("node-a", "srv-a"))

	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(api.routeCalls) != 0 {
		t.Errorf("expected no route calls, got: %+v", api.routeCalls)
	}
}

func TestReconcile_SpecChangeRoutesOnlyNewIP(t *testing.T) {
	fip := routedFIP("group-a", "198.51.100.1/32", "198.51.100.2/32")
	fip.Generation = 2 // spec changed since the condition was set at generation 1

	api := &mockAPI{
		servers: []scp.ServerListMinimal{server(t, 101, "srv-a")},
		v4: []scp.FailoverIPv4{
			fv4(t, 11, "198.51.100.1", ptr(int32(101))),
			fv4(t, 12, "198.51.100.2", nil),
		},
	}
	r := newReconciler(t, api, fip, readyNode("node-a", "srv-a"), testSecret())

	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(api.routeCalls) != 1 || api.routeCalls[0].failoverID != 12 {
		t.Fatalf("expected exactly the new IP to be routed, got: %+v", api.routeCalls)
	}
	cond := routedCondition(t, r, "group-a")
	if cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != 2 {
		t.Errorf("Routed condition = %s (gen %d), want True (gen 2)", cond.Status, cond.ObservedGeneration)
	}
}

func TestReconcile_ReroutesWhenNodeFails(t *testing.T) {
	api := &mockAPI{
		servers: []scp.ServerListMinimal{server(t, 101, "srv-a"), server(t, 102, "srv-b")},
		v4:      []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", ptr(int32(101)))},
	}
	r := newReconciler(t, api,
		routedFIP("group-a", "198.51.100.1/32"),
		notReadyNode("node-a"),
		readyNode("node-b", "srv-b"),
		testSecret(),
	)

	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	want := routeCall{family: "v4", failoverID: 11, serverID: 102}
	if len(api.routeCalls) != 1 || api.routeCalls[0] != want {
		t.Fatalf("expected one route call %+v, got: %+v", want, api.routeCalls)
	}
	if got := getFIP(t, r, "group-a"); got.Status.CurrentNode != "node-b" {
		t.Errorf("currentNode = %q, want node-b", got.Status.CurrentNode)
	}
}

func TestReconcile_CredentialsErrors(t *testing.T) {
	brokenSecret := testSecret()
	delete(brokenSecret.Data, "token")

	tests := []struct {
		name    string
		objs    []client.Object
		connect func([]byte, func([]byte)) (*netcup.Session, error)
	}{
		{name: "secret missing"},
		{name: "token key missing", objs: []client.Object{brokenSecret}},
		{
			name: "connect fails",
			objs: []client.Object{testSecret()},
			connect: func([]byte, func([]byte)) (*netcup.Session, error) {
				return nil, errors.New("parsing OAuth token: bad")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &mockAPI{}
			objs := append([]client.Object{testFIP("group-a", "198.51.100.1/32"), readyNode("node-a", "srv-a")}, tt.objs...)
			r := newReconciler(t, api, objs...)
			if tt.connect != nil {
				r.Connect = tt.connect
			}

			if _, err := reconcileOnce(t, r, "group-a"); err == nil {
				t.Fatal("expected an error so the reconcile is retried with backoff, got nil")
			}
			if cond := routedCondition(t, r, "group-a"); cond.Reason != netcupv1alpha1.ReasonCredentialsError {
				t.Errorf("condition reason = %s, want %s", cond.Reason, netcupv1alpha1.ReasonCredentialsError)
			}
		})
	}
}

func TestReconcile_PersistsRefreshedToken(t *testing.T) {
	api := &mockAPI{
		servers: []scp.ServerListMinimal{server(t, 101, "srv-a")},
		v4:      []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)},
	}
	r := newReconciler(t, api, testFIP("group-a", "198.51.100.1/32"), readyNode("node-a", "srv-a"), testSecret())
	// Simulate the IdP rotating the token mid-session: the session fires the
	// refresh callback, which must land in the Secret.
	inner := r.Connect
	r.Connect = func(tokenJSON []byte, onRefresh func([]byte)) (*netcup.Session, error) {
		onRefresh([]byte(`{"refresh_token":"rotated"}`))
		return inner(tokenJSON, onRefresh)
	}

	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	var secret corev1.Secret
	if err := r.Get(t.Context(), types.NamespacedName{Name: "netcup-credentials", Namespace: "default"}, &secret); err != nil {
		t.Fatalf("getting secret: %v", err)
	}
	if got := string(secret.Data["token"]); got != `{"refresh_token":"rotated"}` {
		t.Errorf("secret token = %q, want the rotated token", got)
	}
}

func TestReconcile_NoEligibleNodes(t *testing.T) {
	r := newReconciler(t, &mockAPI{}, testFIP("group-a", "198.51.100.1/32"), notReadyNode("node-a"))

	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error (node events re-enqueue), got: %v", err)
	}
	if cond := routedCondition(t, r, "group-a"); cond.Reason != netcupv1alpha1.ReasonNoEligibleNodes {
		t.Errorf("condition reason = %s, want %s", cond.Reason, netcupv1alpha1.ReasonNoEligibleNodes)
	}
}

func TestReconcile_NoAccountNodes(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
	}{
		{name: "node without annotation", node: readyNode("node-a", "")},
		{name: "annotated server not in account", node: readyNode("node-a", "srv-foreign")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &mockAPI{servers: []scp.ServerListMinimal{server(t, 101, "srv-a")}}
			r := newReconciler(t, api, testFIP("group-a", "198.51.100.1/32"), tt.node, testSecret())

			if _, err := reconcileOnce(t, r, "group-a"); err != nil {
				t.Fatalf("expected no error (annotating triggers a node event), got: %v", err)
			}
			if cond := routedCondition(t, r, "group-a"); cond.Reason != netcupv1alpha1.ReasonNoAccountNodes {
				t.Errorf("condition reason = %s, want %s", cond.Reason, netcupv1alpha1.ReasonNoAccountNodes)
			}
			if len(api.routeCalls) != 0 {
				t.Errorf("expected no route calls, got: %+v", api.routeCalls)
			}
		})
	}
}

func TestReconcile_MultiAccountPicksThisAccountsNode(t *testing.T) {
	// node-a is backed by another netcup account's server; the group's
	// credentials only know srv-b, so selection must skip node-a regardless
	// of hashing order.
	api := &mockAPI{
		servers: []scp.ServerListMinimal{server(t, 102, "srv-b")},
		v4:      []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)},
	}
	r := newReconciler(t, api,
		testFIP("group-a", "198.51.100.1/32"),
		readyNode("node-a", "srv-of-other-account"),
		readyNode("node-b", "srv-b"),
		testSecret(),
	)

	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	want := routeCall{family: "v4", failoverID: 11, serverID: 102}
	if len(api.routeCalls) != 1 || api.routeCalls[0] != want {
		t.Fatalf("expected one route call %+v, got: %+v", want, api.routeCalls)
	}
	if got := getFIP(t, r, "group-a"); got.Status.CurrentNode != "node-b" {
		t.Errorf("currentNode = %q, want node-b", got.Status.CurrentNode)
	}
}

func TestReconcile_InvalidCIDR(t *testing.T) {
	api := &mockAPI{}
	r := newReconciler(t, api, testFIP("group-a", "not-a-cidr"), readyNode("node-a", "srv-a"), testSecret())

	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error (spec fix bumps generation), got: %v", err)
	}
	if cond := routedCondition(t, r, "group-a"); cond.Reason != netcupv1alpha1.ReasonInvalidIP {
		t.Errorf("condition reason = %s, want %s", cond.Reason, netcupv1alpha1.ReasonInvalidIP)
	}
}

func TestReconcile_ServerLookupError(t *testing.T) {
	api := &mockAPI{listServersErr: errors.New("boom")}
	r := newReconciler(t, api, testFIP("group-a", "198.51.100.1/32"), readyNode("node-a", "srv-a"), testSecret())

	if _, err := reconcileOnce(t, r, "group-a"); err == nil {
		t.Fatal("expected an error, got nil")
	}
	if cond := routedCondition(t, r, "group-a"); cond.Reason != netcupv1alpha1.ReasonServerLookupError {
		t.Errorf("condition reason = %s, want %s", cond.Reason, netcupv1alpha1.ReasonServerLookupError)
	}
}

func TestReconcile_UnknownFailoverIP(t *testing.T) {
	api := &mockAPI{
		servers: []scp.ServerListMinimal{server(t, 101, "srv-a")},
		v4:      []scp.FailoverIPv4{fv4(t, 11, "203.0.113.9", nil)}, // account has a different IP
	}
	r := newReconciler(t, api, testFIP("group-a", "198.51.100.1/32"), readyNode("node-a", "srv-a"), testSecret())

	if _, err := reconcileOnce(t, r, "group-a"); err == nil {
		t.Fatal("expected an error so a newly assigned IP is retried, got nil")
	}
	if cond := routedCondition(t, r, "group-a"); cond.Reason != netcupv1alpha1.ReasonRoutingFailed {
		t.Errorf("condition reason = %s, want %s", cond.Reason, netcupv1alpha1.ReasonRoutingFailed)
	}
}

func TestReconcile_RateLimitResumesWithoutReroutingDoneIPs(t *testing.T) {
	api := &mockAPI{
		servers: []scp.ServerListMinimal{server(t, 101, "srv-a")},
		v4: []scp.FailoverIPv4{
			fv4(t, 11, "198.51.100.1", nil),
			fv4(t, 12, "198.51.100.2", nil),
		},
		routeErr: func(call routeCall) error {
			if call.failoverID == 12 {
				return errors.New("route failover ipv4: unexpected status code: 429")
			}
			return nil
		},
	}
	r := newReconciler(t, api,
		testFIP("group-a", "198.51.100.1/32", "198.51.100.2/32"),
		readyNode("node-a", "srv-a"),
		testSecret(),
	)

	res, err := reconcileOnce(t, r, "group-a")
	if err != nil {
		t.Fatalf("expected no error on rate limit, got: %v", err)
	}
	if res.RequeueAfter != rateLimitRequeue {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, rateLimitRequeue)
	}
	if cond := routedCondition(t, r, "group-a"); cond.Reason != netcupv1alpha1.ReasonRateLimited {
		t.Errorf("condition reason = %s, want %s", cond.Reason, netcupv1alpha1.ReasonRateLimited)
	}

	// After the rate-limit window, only the remaining IP is routed: the first
	// one already shows as routed in the API state.
	api.routeErr = nil
	api.routeCalls = nil
	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(api.routeCalls) != 1 || api.routeCalls[0].failoverID != 12 {
		t.Fatalf("expected only the remaining IP to be routed, got: %+v", api.routeCalls)
	}
	if cond := routedCondition(t, r, "group-a"); cond.Status != metav1.ConditionTrue {
		t.Errorf("Routed condition = %s (%s), want True", cond.Status, cond.Reason)
	}
}

func TestReconcile_TaskError(t *testing.T) {
	api := &mockAPI{
		servers:   []scp.ServerListMinimal{server(t, 101, "srv-a")},
		v4:        []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", nil)},
		taskState: scp.TaskStateERROR,
	}
	r := newReconciler(t, api, testFIP("group-a", "198.51.100.1/32"), readyNode("node-a", "srv-a"), testSecret())

	if _, err := reconcileOnce(t, r, "group-a"); err == nil {
		t.Fatal("expected an error when the routing task fails, got nil")
	}
	if cond := routedCondition(t, r, "group-a"); cond.Reason != netcupv1alpha1.ReasonRoutingFailed {
		t.Errorf("condition reason = %s, want %s", cond.Reason, netcupv1alpha1.ReasonRoutingFailed)
	}
}
