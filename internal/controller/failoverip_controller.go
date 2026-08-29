// Package controller reconciles NetcupFailoverIP resources: it selects a
// healthy node per group and drives the netcup SCP API (via internal/netcup)
// until every failover IP is routed to that node's server.
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	netcupv1alpha1 "github.com/digilolnet/netcup-failover-controller/api/v1alpha1"
	"github.com/digilolnet/netcup-failover-controller/internal/netcup"
)

const (
	// annotationServerName names the SCP server backing a node.
	annotationServerName = "netcup.digilol.net/server-name"
	// rateLimitRequeue matches the netcup failover routing rate-limit window
	// (10 requests per 5 minutes).
	rateLimitRequeue = 5 * time.Minute
)

type FailoverIPReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Connect opens an authenticated netcup SCP session; tests swap it for a
	// stub returning a mock API.
	Connect func(tokenJSON []byte, onRefresh func(tokenJSON []byte)) (*netcup.Session, error)
	// CredentialsNamespace is the namespace credentials Secrets live in —
	// the controller's own, matching the namespaced Role it holds on Secrets.
	CredentialsNamespace string
}

func (r *FailoverIPReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var fip netcupv1alpha1.NetcupFailoverIP
	if err := r.Get(ctx, req.NamespacedName, &fip); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	nodes, err := r.eligibleNodes(ctx, fip.Spec.NodeSelector)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(nodes) == 0 {
		// Node events re-enqueue all resources, so a node becoming ready
		// triggers a new reconcile; no requeue needed.
		return ctrl.Result{}, r.setCondition(ctx, &fip, metav1.ConditionFalse,
			netcupv1alpha1.ReasonNoEligibleNodes, "no healthy nodes match selector")
	}

	// Stay on the current node as long as it remains healthy and eligible,
	// routing is complete, and the spec has not changed since.
	currentEligible := nodeByName(nodes, fip.Status.CurrentNode).Name != ""
	if currentEligible && isRouted(&fip) {
		return ctrl.Result{}, nil
	}

	routes, err := netcup.ParseRoutes(fip.Spec.IPs)
	if err != nil {
		// A spec problem: retrying cannot help. The user fixing the spec bumps
		// the generation and triggers a new reconcile.
		return ctrl.Result{}, r.setCondition(ctx, &fip, metav1.ConditionFalse,
			netcupv1alpha1.ReasonInvalidIP, err.Error())
	}

	session, err := r.connect(ctx, fip.Spec.CredentialsSecret.Name)
	if err != nil {
		// Secrets are not watched, so return the error to retry with backoff.
		return ctrl.Result{}, errors.Join(err, r.setCondition(ctx, &fip, metav1.ConditionFalse,
			netcupv1alpha1.ReasonCredentialsError, err.Error()))
	}
	defer session.Close()

	serverIDs, err := session.ServerIDsByName(ctx)
	if err != nil {
		return ctrl.Result{}, errors.Join(err, r.setCondition(ctx, &fip, metav1.ConditionFalse,
			netcupv1alpha1.ReasonServerLookupError, err.Error()))
	}

	// A group may only be placed where its account can route: multi-account
	// clusters mix nodes backed by servers of different netcup accounts.
	accountNodes := nodesInAccount(nodes, serverIDs)
	if len(accountNodes) == 0 {
		// Annotating or relabeling a node triggers a node event.
		return ctrl.Result{}, r.setCondition(ctx, &fip, metav1.ConditionFalse,
			netcupv1alpha1.ReasonNoAccountNodes,
			fmt.Sprintf("none of the %d eligible nodes has annotation %s naming a server of the netcup account",
				len(nodes), annotationServerName))
	}

	node := nodeByName(accountNodes, fip.Status.CurrentNode)
	if node.Name == "" {
		occupied, err := r.occupiedNodes(ctx, &fip)
		if err != nil {
			return ctrl.Result{}, err
		}
		node = selectNode(fip.Name, accountNodes, occupied)
	}
	serverName := node.Annotations[annotationServerName]
	serverID := serverIDs[serverName]

	// Record the target before routing so occupiedNodes spreads other groups
	// correctly during the transition and requeues keep the same target.
	if fip.Status.CurrentNode != node.Name {
		fip.Status.CurrentNode = node.Name
		if err := r.setCondition(ctx, &fip, metav1.ConditionFalse,
			netcupv1alpha1.ReasonRouting, fmt.Sprintf("routing to node %s", node.Name)); err != nil {
			return ctrl.Result{}, err
		}
	}

	log.Info("routing failover IPs", "ips", fip.Spec.IPs, "node", node.Name, "server", serverName, "serverID", serverID)

	if err := session.EnsureRouted(ctx, routes, serverID); err != nil {
		if netcup.IsRateLimited(err) {
			log.Info("netcup rate limit hit, requeueing", "requeue", rateLimitRequeue)
			return ctrl.Result{RequeueAfter: rateLimitRequeue}, r.setCondition(ctx, &fip, metav1.ConditionFalse,
				netcupv1alpha1.ReasonRateLimited, "waiting out the netcup failover routing rate limit")
		}
		return ctrl.Result{}, errors.Join(err, r.setCondition(ctx, &fip, metav1.ConditionFalse,
			netcupv1alpha1.ReasonRoutingFailed, err.Error()))
	}

	return ctrl.Result{}, r.setCondition(ctx, &fip, metav1.ConditionTrue,
		netcupv1alpha1.ReasonNodeSelected, fmt.Sprintf("routed to node %s", node.Name))
}

// isRouted reports whether routing completed for the current spec generation.
func isRouted(fip *netcupv1alpha1.NetcupFailoverIP) bool {
	cond := meta.FindStatusCondition(fip.Status.Conditions, netcupv1alpha1.ConditionRouted)
	return cond != nil && cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == fip.Generation
}

// connect reads the OAuth token from the named credentials Secret in the
// controller's namespace and opens an SCP session. Refreshed tokens are
// written back to the Secret so the stored refresh token never expires from
// disuse.
func (r *FailoverIPReconciler) connect(ctx context.Context, name string) (*netcup.Session, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.CredentialsNamespace, Name: name}, &secret); err != nil {
		return nil, fmt.Errorf("reading credentials secret %s/%s: %w", r.CredentialsNamespace, name, err)
	}
	tokenJSON := secret.Data[netcup.TokenSecretKey]
	if len(tokenJSON) == 0 {
		return nil, fmt.Errorf("credentials secret %s/%s missing key %q (seed it with `netcup-failover-controller login --secret %s/%s`)",
			r.CredentialsNamespace, name, netcup.TokenSecretKey, r.CredentialsNamespace, name)
	}
	return r.Connect(tokenJSON, func(refreshed []byte) {
		r.persistToken(ctx, name, refreshed)
	})
}

// persistToken stores a refreshed OAuth token back into the credentials
// Secret. Failures are logged, not returned: the reconcile that triggered the
// refresh still holds a valid access token and should proceed.
func (r *FailoverIPReconciler) persistToken(ctx context.Context, name string, tokenJSON []byte) {
	log := logf.FromContext(ctx)
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.CredentialsNamespace, Name: name}, &secret); err != nil {
		log.Error(err, "reading credentials secret to persist refreshed token", "secret", name)
		return
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[netcup.TokenSecretKey] = tokenJSON
	if err := r.Update(ctx, &secret); err != nil {
		log.Error(err, "persisting refreshed token to credentials secret", "secret", name)
	}
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

func (r *FailoverIPReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&netcupv1alpha1.NetcupFailoverIP{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueAll),
			builder.WithPredicates(nodePredicate()),
		).
		Complete(r)
}

func (r *FailoverIPReconciler) enqueueAll(ctx context.Context, _ client.Object) []reconcile.Request {
	var list netcupv1alpha1.NetcupFailoverIPList
	if err := r.List(ctx, &list); err != nil {
		logf.FromContext(ctx).Error(err, "listing NetcupFailoverIPs for node event")
		return nil
	}
	reqs := make([]reconcile.Request, len(list.Items))
	for i, item := range list.Items {
		reqs[i] = reconcile.Request{NamespacedName: types.NamespacedName{Name: item.Name}}
	}
	return reqs
}
