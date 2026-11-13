package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digilolnet/go-netcup-scp/pkg/scp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	netcupv1alpha1 "github.com/digilolnet/netcup-failover-controller/api/v1alpha1"
	"github.com/digilolnet/netcup-failover-controller/internal/netcup"
)

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
	task.Uuid = new("task-1")
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
	return newReconcilerWithInterceptor(t, api, interceptor.Funcs{}, objs...)
}

// newReconcilerWithInterceptor is newReconciler with hooks into the fake
// client, for simulating API server failures.
func newReconcilerWithInterceptor(
	t *testing.T,
	api *mockAPI,
	funcs interceptor.Funcs,
	objs ...client.Object,
) *FailoverIPReconciler {
	t.Helper()
	api.t = t
	cl := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&netcupv1alpha1.NetcupFailoverIP{}).
		WithInterceptorFuncs(funcs).
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
	if !slices.Equal(api.routeCalls, want) {
		t.Fatalf("route calls = %+v, want %+v", api.routeCalls, want)
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
		v4:      []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", new(int32(101)))}, // already on target
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
			fv4(t, 11, "198.51.100.1", new(int32(101))),
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
		v4:      []scp.FailoverIPv4{fv4(t, 11, "198.51.100.1", new(int32(101)))},
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
	if got := secretToken(t, r); got != `{"refresh_token":"rotated"}` {
		t.Errorf("secret token = %q, want the rotated token", got)
	}
}

func secretToken(t *testing.T, r *FailoverIPReconciler) string {
	t.Helper()
	var secret corev1.Secret
	if err := r.Get(t.Context(), types.NamespacedName{Name: "netcup-credentials", Namespace: "default"}, &secret); err != nil {
		t.Fatalf("getting secret: %v", err)
	}
	return string(secret.Data["token"])
}

// A token refreshed just as the reconcile is canceled (shutdown, lost
// leadership) must still be saved: the IdP may already have invalidated the
// previous refresh token.
func TestPersistToken_SurvivesCanceledContext(t *testing.T) {
	// The fake client ignores contexts; make it honor them like a real one.
	funcs := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
	}
	r := newReconcilerWithInterceptor(t, &mockAPI{}, funcs, testSecret())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.persistToken(ctx, "netcup-credentials", []byte(`{"refresh_token":"rotated"}`))

	if got := secretToken(t, r); got != `{"refresh_token":"rotated"}` {
		t.Errorf("secret token = %q, want the rotated token", got)
	}
}

// A concurrent writer (e.g. `login`) must not make the controller drop a
// refreshed token.
func TestPersistToken_RetriesOnConflict(t *testing.T) {
	conflicts := 1
	funcs := interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if conflicts > 0 {
				conflicts--
				return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), errors.New("stale"))
			}
			return c.Update(ctx, obj, opts...)
		},
	}
	r := newReconcilerWithInterceptor(t, &mockAPI{}, funcs, testSecret())

	r.persistToken(t.Context(), "netcup-credentials", []byte(`{"refresh_token":"rotated"}`))

	if got := secretToken(t, r); got != `{"refresh_token":"rotated"}` {
		t.Errorf("secret token = %q, want the rotated token", got)
	}
}

func TestReconcile_SpreadsGroupsAcrossNodes(t *testing.T) {
	api := &mockAPI{
		servers: []scp.ServerListMinimal{server(t, 101, "srv-a"), server(t, 102, "srv-b")},
		v4: []scp.FailoverIPv4{
			fv4(t, 11, "198.51.100.1", new(int32(101))),
			fv4(t, 12, "198.51.100.2", nil),
		},
	}
	r := newReconciler(t, api,
		routedFIP("group-a", "198.51.100.1/32"), // already on node-a
		testFIP("group-b", "198.51.100.2/32"),
		readyNode("node-a", "srv-a"),
		readyNode("node-b", "srv-b"),
		testSecret(),
	)

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "group-b"}}); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if got := getFIP(t, r, "group-b").Status.CurrentNode; got != "node-b" {
		t.Errorf("group-b currentNode = %q, want the unoccupied node-b", got)
	}
	want := []routeCall{{family: "v4", failoverID: 12, serverID: 102}}
	if !slices.Equal(api.routeCalls, want) {
		t.Errorf("route calls = %+v, want %+v", api.routeCalls, want)
	}
}

// Upstream errors can embed whole response bodies; an oversized condition
// message would make the API server reject the status update.
func TestReconcile_TruncatesLongConditionMessage(t *testing.T) {
	api := &mockAPI{listServersErr: errors.New(strings.Repeat("é", 5000))}
	r := newReconciler(t, api, testFIP("group-a", "198.51.100.1/32"), readyNode("node-a", "srv-a"), testSecret())

	if _, err := reconcileOnce(t, r, "group-a"); err == nil {
		t.Fatal("expected an error, got nil")
	}
	msg := routedCondition(t, r, "group-a").Message
	if len(msg) > maxConditionMessage+len("…") {
		t.Errorf("condition message is %d bytes, want at most %d", len(msg), maxConditionMessage+len("…"))
	}
	if !utf8.ValidString(msg) || !strings.HasSuffix(msg, "…") {
		t.Errorf("condition message must stay valid UTF-8 and end with an ellipsis, got suffix %q", msg[len(msg)-8:])
	}
}

func TestEnqueueAll(t *testing.T) {
	r := newReconciler(t, &mockAPI{}, testFIP("group-a", "198.51.100.1/32"), testFIP("group-b", "198.51.100.2/32"))

	reqs := r.enqueueAll(t.Context(), readyNode("node-a", "srv-a"))

	got := make([]string, len(reqs))
	for i, req := range reqs {
		if req.Namespace != "" {
			t.Errorf("request %v has a namespace; NetcupFailoverIP is cluster-scoped", req)
		}
		got[i] = req.Name
	}
	slices.Sort(got)
	if want := []string{"group-a", "group-b"}; !slices.Equal(got, want) {
		t.Errorf("enqueued %v, want %v", got, want)
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

func TestReconcile_InvalidNodeSelector(t *testing.T) {
	fip := testFIP("group-a", "198.51.100.1/32")
	fip.Spec.NodeSelector = &metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "role", Operator: metav1.LabelSelectorOpIn}, // In requires values
		},
	}
	r := newReconciler(t, &mockAPI{}, fip, readyNode("node-a", "srv-a"), testSecret())

	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error (spec fix bumps generation), got: %v", err)
	}
	if cond := routedCondition(t, r, "group-a"); cond.Reason != netcupv1alpha1.ReasonInvalidNodeSelector {
		t.Errorf("condition reason = %s, want %s", cond.Reason, netcupv1alpha1.ReasonInvalidNodeSelector)
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

// drainEvents returns the "<type> <reason>" of every event recorded so far.
func drainEvents(rec *events.FakeRecorder) []string {
	var got []string
	for {
		select {
		case e := <-rec.Events:
			fields := strings.Fields(e)
			got = append(got, fields[0]+" "+fields[1])
		default:
			return got
		}
	}
}

func condition(t *testing.T, r *FailoverIPReconciler, name, conditionType string) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(getFIP(t, r, name).Status.Conditions, conditionType)
}

func TestReconcile_Resync(t *testing.T) {
	const interval = time.Hour
	srvA, srvB := int32(101), int32(102)

	newResyncing := func(t *testing.T, api *mockAPI) (*FailoverIPReconciler, *events.FakeRecorder) {
		t.Helper()
		r := newReconciler(t, api, routedFIP("group-a", "198.51.100.1/32"),
			readyNode("node-a", "srv-a"), testSecret())
		rec := events.NewFakeRecorder(10)
		r.ResyncInterval = interval
		r.Recorder = rec
		return r, rec
	}

	t.Run("routing intact records the check and schedules the next", func(t *testing.T) {
		api := &mockAPI{
			servers: []scp.ServerListMinimal{server(t, srvA, "srv-a")},
			v4:      []scp.FailoverIPv4{fv4(t, 1, "198.51.100.1", &srvA)},
		}
		r, rec := newResyncing(t, api)

		res, err := reconcileOnce(t, r, "group-a")
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if res.RequeueAfter != interval {
			t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, interval)
		}
		if len(api.routeCalls) != 0 {
			t.Errorf("expected no route calls, got: %+v", api.routeCalls)
		}
		if got := getFIP(t, r, "group-a").Status.LastVerifiedTime; got == nil || time.Since(got.Time) > time.Minute {
			t.Errorf("lastVerifiedTime = %v, want about now", got)
		}
		if cond := condition(t, r, "group-a", netcupv1alpha1.ConditionVerified); cond == nil ||
			cond.Status != metav1.ConditionTrue {
			t.Errorf("Verified = %+v, want True", cond)
		}
		if got := drainEvents(rec); len(got) != 0 {
			t.Errorf("an uneventful check recorded events: %v", got)
		}

		// Until the interval elapses a reconcile (e.g. from a Node event)
		// must not ask netcup again.
		api.listServersErr = errors.New("netcup must not be asked again")
		res, err = reconcileOnce(t, r, "group-a")
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if res.RequeueAfter <= 0 || res.RequeueAfter > interval {
			t.Errorf("RequeueAfter = %v, want the remainder of %v", res.RequeueAfter, interval)
		}
	})

	t.Run("a check from before the interval is repeated", func(t *testing.T) {
		api := &mockAPI{
			servers: []scp.ServerListMinimal{server(t, srvA, "srv-a")},
			v4:      []scp.FailoverIPv4{fv4(t, 1, "198.51.100.1", &srvA)},
		}
		fip := routedFIP("group-a", "198.51.100.1/32")
		old := metav1.NewTime(time.Now().Add(-2 * interval))
		fip.Status.LastVerifiedTime = &old
		r := newReconciler(t, api, fip, readyNode("node-a", "srv-a"), testSecret())
		r.ResyncInterval = interval

		if _, err := reconcileOnce(t, r, "group-a"); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if got := getFIP(t, r, "group-a").Status.LastVerifiedTime; got == nil || !got.After(old.Time) {
			t.Errorf("lastVerifiedTime = %v, want it advanced past %v", got, old)
		}
	})

	t.Run("drift is re-routed", func(t *testing.T) {
		api := &mockAPI{
			servers: []scp.ServerListMinimal{server(t, srvA, "srv-a"), server(t, srvB, "srv-b")},
			v4:      []scp.FailoverIPv4{fv4(t, 1, "198.51.100.1", &srvB)}, // moved in the SCP panel
		}
		r, rec := newResyncing(t, api)

		res, err := reconcileOnce(t, r, "group-a")
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if len(api.routeCalls) != 1 || api.routeCalls[0].serverID != srvA {
			t.Fatalf("expected one route call back to server %d, got: %+v", srvA, api.routeCalls)
		}
		if res.RequeueAfter != interval {
			t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, interval)
		}
		if cond := routedCondition(t, r, "group-a"); cond.Status != metav1.ConditionTrue {
			t.Errorf("Routed = %s (%s), want True after re-routing", cond.Status, cond.Message)
		}
		want := []string{"Warning DriftDetected", "Normal Routed"}
		if got := drainEvents(rec); !slices.Equal(got, want) {
			t.Errorf("events = %v, want %v", got, want)
		}
	})

	t.Run("failed drift repair flips the condition", func(t *testing.T) {
		api := &mockAPI{
			servers:  []scp.ServerListMinimal{server(t, srvA, "srv-a")},
			v4:       []scp.FailoverIPv4{fv4(t, 1, "198.51.100.1", nil)},
			routeErr: func(routeCall) error { return errors.New("unexpected status code: 500") },
		}
		r, rec := newResyncing(t, api)

		if _, err := reconcileOnce(t, r, "group-a"); err == nil {
			t.Fatal("expected the routing error, got nil")
		}
		cond := routedCondition(t, r, "group-a")
		if cond.Status != metav1.ConditionFalse || cond.Reason != netcupv1alpha1.ReasonRoutingFailed {
			t.Errorf("Routed = %s/%s, want False/%s", cond.Status, cond.Reason, netcupv1alpha1.ReasonRoutingFailed)
		}
		if cond := condition(t, r, "group-a", netcupv1alpha1.ConditionVerified); cond != nil {
			t.Errorf("Verified = %+v, want it absent while Routed is False", cond)
		}
		want := []string{"Warning DriftDetected", "Warning RoutingFailed"}
		if got := drainEvents(rec); !slices.Equal(got, want) {
			t.Errorf("events = %v, want %v", got, want)
		}
	})

	t.Run("a failed check keeps Routed, sets Verified=False and is retried", func(t *testing.T) {
		api := &mockAPI{listServersErr: errors.New("scp unavailable")}
		r, _ := newResyncing(t, api)

		for range 2 { // the check stays due, so the next reconcile tries again
			if _, err := reconcileOnce(t, r, "group-a"); err == nil {
				t.Fatal("expected the lookup error, got nil")
			}
		}
		if cond := routedCondition(t, r, "group-a"); cond.Status != metav1.ConditionTrue {
			t.Errorf("Routed = %s/%s, want it to stay True", cond.Status, cond.Reason)
		}
		cond := condition(t, r, "group-a", netcupv1alpha1.ConditionVerified)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != netcupv1alpha1.ReasonServerLookupError {
			t.Errorf("Verified = %+v, want False/%s", cond, netcupv1alpha1.ReasonServerLookupError)
		}
		if got := getFIP(t, r, "group-a").Status.LastVerifiedTime; got != nil {
			t.Errorf("lastVerifiedTime = %v, want it unset after only failed checks", got)
		}

		// Once netcup answers again the condition recovers.
		api.listServersErr = nil
		api.servers = []scp.ServerListMinimal{server(t, srvA, "srv-a")}
		api.v4 = []scp.FailoverIPv4{fv4(t, 1, "198.51.100.1", &srvA)}
		if _, err := reconcileOnce(t, r, "group-a"); err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
		if cond := condition(t, r, "group-a", netcupv1alpha1.ConditionVerified); cond == nil ||
			cond.Status != metav1.ConditionTrue {
			t.Errorf("Verified = %+v, want True after a successful check", cond)
		}
	})
}

func TestReconcile_RecordsEvents(t *testing.T) {
	srvA := int32(101)
	api := &mockAPI{
		servers: []scp.ServerListMinimal{server(t, srvA, "srv-a")},
		v4:      []scp.FailoverIPv4{fv4(t, 1, "198.51.100.1", nil)},
	}
	r := newReconciler(t, api, testFIP("group-a", "198.51.100.1/32"), readyNode("node-a", "srv-a"), testSecret())
	rec := events.NewFakeRecorder(10)
	r.Recorder = rec

	if _, err := reconcileOnce(t, r, "group-a"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if got, want := drainEvents(rec), []string{"Normal Routed"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}

	// An outcome nothing but a spec or node change can fix is a warning.
	r2 := newReconciler(t, &mockAPI{}, testFIP("group-b", "198.51.100.1/32"))
	rec2 := events.NewFakeRecorder(10)
	r2.Recorder = rec2
	if _, err := reconcileOnce(t, r2, "group-b"); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if got, want := drainEvents(rec2), []string{"Warning NoEligibleNodes"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}
