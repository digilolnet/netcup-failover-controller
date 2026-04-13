package controller

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	netcupv1alpha1 "github.com/digilolnet/netcup-failover-controller/api/v1alpha1"
	"github.com/digilolnet/netcup-failover-controller/internal/netcup"
)

const (
	annotationServerName = "netcup.digilol.net/server-name"
	retryCount           = 10
	rateLimitMsg         = "Routing a failover IP is only permitted once every 5 minutes."
	rateLimitRequeue     = 5 * time.Minute
)

type FailoverIPReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	SOAP          netcup.SOAPAPI
	RetryInterval time.Duration
}

func (r *FailoverIPReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var fip netcupv1alpha1.NetcupFailoverIP
	if err := r.Get(ctx, req.NamespacedName, &fip); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	loginName, password, err := r.readCredentials(ctx, fip.Spec.CredentialsSecret)
	if err != nil {
		return ctrl.Result{}, r.setCondition(ctx, &fip, metav1.ConditionFalse, "CredentialsError", err.Error())
	}

	nodes, err := r.eligibleNodes(ctx, fip.Spec.NodeSelector)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(nodes) == 0 {
		return ctrl.Result{}, r.setCondition(ctx, &fip, metav1.ConditionFalse,
			"NoEligibleNodes", "no healthy nodes match selector")
	}

	// Stay on the current node as long as it remains healthy and eligible.
	// Only re-route when the node actually fails or leaves the eligible set.
	for _, n := range nodes {
		if n.Name == fip.Status.CurrentNode {
			return ctrl.Result{}, nil
		}
	}

	occupied, err := r.occupiedNodes(ctx, &fip)
	if err != nil {
		return ctrl.Result{}, err
	}

	node := selectNode(fip.Name, nodes, occupied)

	serverName, ok := node.Annotations[annotationServerName]
	if !ok || serverName == "" {
		return ctrl.Result{}, r.setCondition(ctx, &fip, metav1.ConditionFalse,
			"MissingAnnotation", fmt.Sprintf("node %s missing annotation %s", node.Name, annotationServerName))
	}

	info, err := r.SOAP.GetServerInfo(ctx, loginName, password, serverName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting server info for %s: %w", serverName, err)
	}

	slog.Info("routing failover IPs", "ips", fip.Spec.IPs, "node", node.Name, "server", serverName)

	for _, cidr := range fip.Spec.IPs {
		ip, netmask, err := parseCIDR(cidr)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.routeWithRetry(ctx, loginName, password, ip, netmask, serverName, info.MAC); err != nil {
			if errors.Is(err, errRateLimit) {
				slog.Warn("rate limited, requeuing", "ip", ip, "requeue", rateLimitRequeue)
				return ctrl.Result{RequeueAfter: rateLimitRequeue}, nil
			}
			return ctrl.Result{}, r.setCondition(ctx, &fip, metav1.ConditionFalse, "RoutingFailed", err.Error())
		}
	}

	return ctrl.Result{}, r.setRoutedCondition(ctx, &fip, node.Name)
}

func (r *FailoverIPReconciler) readCredentials(ctx context.Context, ref corev1.SecretReference) (loginName, password string, err error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, &secret); err != nil {
		return "", "", fmt.Errorf("reading credentials secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	loginName = string(secret.Data["loginName"])
	password = string(secret.Data["password"])
	if loginName == "" {
		return "", "", fmt.Errorf("credentials secret %s/%s missing key loginName", ref.Namespace, ref.Name)
	}
	if password == "" {
		return "", "", fmt.Errorf("credentials secret %s/%s missing key password", ref.Namespace, ref.Name)
	}
	return loginName, password, nil
}

func (r *FailoverIPReconciler) eligibleNodes(ctx context.Context, selector *metav1.LabelSelector) ([]corev1.Node, error) {
	listOpts := []client.ListOption{}
	if selector != nil {
		sel, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return nil, fmt.Errorf("invalid nodeSelector: %w", err)
		}
		listOpts = append(listOpts, client.MatchingLabelsSelector{Selector: sel})
	}

	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList, listOpts...); err != nil {
		return nil, err
	}

	var eligible []corev1.Node
	for _, node := range nodeList.Items {
		if isNodeReady(&node) {
			eligible = append(eligible, node)
		}
	}
	return eligible, nil
}

func (r *FailoverIPReconciler) occupiedNodes(ctx context.Context, current *netcupv1alpha1.NetcupFailoverIP) (map[string]bool, error) {
	var list netcupv1alpha1.NetcupFailoverIPList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	occupied := make(map[string]bool)
	for _, fip := range list.Items {
		if fip.Name == current.Name {
			continue
		}
		if fip.Status.CurrentNode != "" {
			occupied[fip.Status.CurrentNode] = true
		}
	}
	return occupied, nil
}

var errRateLimit = fmt.Errorf("netcup rate limit: %s", rateLimitMsg)

func (r *FailoverIPReconciler) routeWithRetry(ctx context.Context, loginName, password, ip, netmask, serverName, mac string) error {
	var lastErr error
	for i := range retryCount {
		lastErr = r.SOAP.ChangeIPRouting(ctx, loginName, password, ip, netmask, serverName, mac)
		if lastErr == nil {
			return nil
		}
		if strings.Contains(lastErr.Error(), rateLimitMsg) {
			return errRateLimit
		}
		if i < retryCount-1 {
			slog.Warn("ChangeIPRouting failed, retrying", "attempt", i+1, "err", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(r.RetryInterval):
			}
		}
	}
	return fmt.Errorf("routing %s after %d attempts: %w", ip, retryCount, lastErr)
}

func (r *FailoverIPReconciler) setCondition(ctx context.Context, fip *netcupv1alpha1.NetcupFailoverIP, status metav1.ConditionStatus, reason, message string) error {
	meta.SetStatusCondition(&fip.Status.Conditions, metav1.Condition{
		Type:               netcupv1alpha1.ConditionRouted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: fip.Generation,
	})
	return r.Status().Update(ctx, fip)
}

func (r *FailoverIPReconciler) setRoutedCondition(ctx context.Context, fip *netcupv1alpha1.NetcupFailoverIP, nodeName string) error {
	fip.Status.CurrentNode = nodeName
	meta.SetStatusCondition(&fip.Status.Conditions, metav1.Condition{
		Type:               netcupv1alpha1.ConditionRouted,
		Status:             metav1.ConditionTrue,
		Reason:             "NodeSelected",
		Message:            fmt.Sprintf("routed to node %s", nodeName),
		ObservedGeneration: fip.Generation,
	})
	return r.Status().Update(ctx, fip)
}

func (r *FailoverIPReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&netcupv1alpha1.NetcupFailoverIP{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueAll),
		).
		Complete(r)
}

func (r *FailoverIPReconciler) enqueueAll(ctx context.Context, _ client.Object) []reconcile.Request {
	var list netcupv1alpha1.NetcupFailoverIPList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, len(list.Items))
	for i, item := range list.Items {
		reqs[i] = reconcile.Request{NamespacedName: types.NamespacedName{Name: item.Name}}
	}
	return reqs
}

// selectNode picks a node deterministically, preferring nodes not already
// hosting another failover IP group.
func selectNode(name string, nodes []corev1.Node, occupied map[string]bool) corev1.Node {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	var free []corev1.Node
	for _, n := range nodes {
		if !occupied[n.Name] {
			free = append(free, n)
		}
	}
	pool := nodes
	if len(free) > 0 {
		pool = free
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return pool[h.Sum32()%uint32(len(pool))]
}

func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func parseCIDR(cidr string) (ip, mask string, err error) {
	addr, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	ones, _ := network.Mask.Size()
	return addr.String(), fmt.Sprintf("%d", ones), nil
}
