// Package controller reconciles NetcupFailoverIP resources: it selects a
// healthy node per group and drives the netcup SCP API (via internal/netcup)
// until every failover IP is routed to that node's server.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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
	// persistTokenTimeout bounds writing a refreshed token back to the
	// credentials Secret.
	persistTokenTimeout = 10 * time.Second
	// maxConditionMessage caps the Routed condition message. Upstream errors
	// can embed whole response bodies, and an oversized message would make
	// the API server reject the status update itself. The full error is
	// still logged through the returned error.
	maxConditionMessage = 1024
)

// FailoverIPReconciler reconciles NetcupFailoverIP resources.
type FailoverIPReconciler struct {
	client.Client
	// Connect opens an authenticated netcup SCP session; tests swap it for a
	// stub returning a mock API.
	Connect func(tokenJSON []byte, onRefresh func(tokenJSON []byte)) (*netcup.Session, error)
	// CredentialsNamespace is the namespace credentials Secrets live in —
	// the controller's own, matching the namespaced Role it holds on Secrets.
	CredentialsNamespace string
	// ResyncInterval is how often a routed group is compared against netcup's
	// actual routing, to catch changes made outside the controller. Zero
	// disables the check: a routed group is then only revisited on spec or
	// Node changes.
	ResyncInterval time.Duration

	// verified records when each group's routing was last confirmed against
	// netcup. It is in-memory on purpose: a new leader starts empty and so
	// verifies every group once at startup.
	mu       sync.Mutex
	verified map[string]time.Time
}

// Reconcile routes the group's failover IPs to a healthy eligible node. While
// the group is routed at the current generation and its node is still
// eligible it does nothing, except once per ResyncInterval, when it checks
// that netcup still routes the IPs there and re-routes them if not.
func (r *FailoverIPReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var fip netcupv1alpha1.NetcupFailoverIP
	if err := r.Get(ctx, req.NamespacedName, &fip); err != nil {
		if apierrors.IsNotFound(err) {
			r.forgetVerified(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	selector, err := nodeSelector(fip.Spec.NodeSelector)
	if err != nil {
		// A spec problem, like an invalid CIDR below: retrying cannot help.
		return ctrl.Result{}, r.unroutable(ctx, &fip, netcupv1alpha1.ReasonInvalidNodeSelector, err.Error())
	}
	nodes, err := r.eligibleNodes(ctx, selector)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(nodes) == 0 {
		// Node events re-enqueue all resources, so a node becoming ready
		// triggers a new reconcile; no requeue needed.
		return ctrl.Result{}, r.unroutable(ctx, &fip,
			netcupv1alpha1.ReasonNoEligibleNodes, "no healthy nodes match selector")
	}

	// Stay on the current node as long as it remains healthy and eligible,
	// routing is complete, and the spec has not changed since.
	// Only a periodic check against netcup's actual routing interrupts that.
	currentEligible := nodeByName(nodes, fip.Status.CurrentNode).Name != ""
	verifying := currentEligible && isRouted(&fip)
	if verifying {
		if wait, due := r.resyncDue(fip.Name); !due {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	routes, err := netcup.ParseRoutes(fip.Spec.IPs)
	if err != nil {
		// A spec problem: retrying cannot help. The user fixing the spec bumps
		// the generation and triggers a new reconcile.
		return ctrl.Result{}, r.unroutable(ctx, &fip, netcupv1alpha1.ReasonInvalidIP, err.Error())
	}

	session, err := r.connect(ctx, fip.Spec.CredentialsSecret.Name)
	if err != nil {
		// Secrets are not watched, so return the error to retry with backoff.
		return ctrl.Result{}, r.fail(ctx, &fip, verifying, netcupv1alpha1.ReasonCredentialsError, err)
	}
	defer session.Close()

	serverIDs, err := session.ServerIDsByName(ctx)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, &fip, verifying, netcupv1alpha1.ReasonServerLookupError, err)
	}

	// A group may only be placed where its account can route: multi-account
	// clusters mix nodes backed by servers of different netcup accounts.
	accountNodes := nodesInAccount(nodes, serverIDs)
	if len(accountNodes) == 0 {
		// Annotating or relabeling a node triggers a node event.
		return ctrl.Result{}, r.unroutable(ctx, &fip, netcupv1alpha1.ReasonNoAccountNodes,
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

	if verifying && node.Name == fip.Status.CurrentNode {
		drifted, err := session.Misrouted(ctx, routes, serverID)
		if err != nil {
			return ctrl.Result{}, r.fail(ctx, &fip, verifying, netcupv1alpha1.ReasonRoutingFailed, err)
		}
		if len(drifted) == 0 {
			return r.routed(fip.Name), nil
		}
		log.Info("netcup no longer routes these failover IPs to the current node, re-routing",
			"ips", drifted, "node", node.Name, "server", serverName)
		if err := r.setCondition(ctx, &fip, metav1.ConditionFalse, netcupv1alpha1.ReasonRouting,
			fmt.Sprintf("netcup routing changed outside the controller; re-routing to node %s", node.Name)); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Record the target before routing so occupiedNodes spreads other groups
	// correctly during the transition and requeues keep the same target.
	if fip.Status.CurrentNode != node.Name {
		fip.Status.CurrentNode = node.Name
		if err := r.setCondition(ctx, &fip, metav1.ConditionFalse,
			netcupv1alpha1.ReasonRouting, fmt.Sprintf("routing to node %s", node.Name)); err != nil {
			return ctrl.Result{}, err
		}
	}

	log.Info("routing failover IPs",
		"ips", fip.Spec.IPs, "node", node.Name, "server", serverName, "serverID", serverID)

	if err := session.EnsureRouted(ctx, routes, serverID); err != nil {
		if netcup.IsRateLimited(err) {
			log.Info("netcup rate limit hit, requeueing", "requeue", rateLimitRequeue)
			return ctrl.Result{RequeueAfter: rateLimitRequeue}, r.setCondition(ctx, &fip, metav1.ConditionFalse,
				netcupv1alpha1.ReasonRateLimited, "waiting out the netcup failover routing rate limit")
		}
		return ctrl.Result{}, r.fail(ctx, &fip, false, netcupv1alpha1.ReasonRoutingFailed, err)
	}

	log.Info("failover IPs routed", "node", node.Name, "server", serverName)
	// The message says what was verified: netcup's routing, not that the node
	// answers on the addresses.
	if err := r.setCondition(ctx, &fip, metav1.ConditionTrue, netcupv1alpha1.ReasonNodeSelected,
		fmt.Sprintf("netcup routes the failover IPs to node %s", node.Name)); err != nil {
		return ctrl.Result{}, err
	}
	return r.routed(fip.Name), nil
}

// resyncDue reports whether the group's routing should be checked against
// netcup now, and otherwise how long until it should. With the check disabled
// it is never due and nothing is requeued.
func (r *FailoverIPReconciler) resyncDue(name string) (wait time.Duration, due bool) {
	if r.ResyncInterval <= 0 {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.verified[name]
	if !ok {
		return 0, true
	}
	if wait = r.ResyncInterval - time.Since(last); wait <= 0 {
		return 0, true
	}
	return wait, false
}

// routed records that netcup's routing was just confirmed for the group and
// returns the result that schedules the next check.
func (r *FailoverIPReconciler) routed(name string) ctrl.Result {
	if r.ResyncInterval <= 0 {
		return ctrl.Result{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.verified == nil {
		r.verified = map[string]time.Time{}
	}
	r.verified[name] = time.Now()
	return ctrl.Result{RequeueAfter: r.ResyncInterval}
}

func (r *FailoverIPReconciler) forgetVerified(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.verified, name)
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
		return nil, fmt.Errorf(
			"credentials secret %s/%s missing key %q (seed it with `netcup-failover-controller login --secret %s/%s`)",
			r.CredentialsNamespace, name, netcup.TokenSecretKey, r.CredentialsNamespace, name)
	}
	return r.Connect(tokenJSON, func(refreshed []byte) {
		r.persistToken(ctx, name, refreshed)
	})
}

// persistToken stores a refreshed OAuth token back into the credentials
// Secret. Failures are logged, not returned: the reconcile that triggered the
// refresh still holds a valid access token and should proceed.
//
// The write is detached from the reconcile's cancellation and retried on
// conflict: the IdP may already have invalidated the previous refresh token,
// so dropping the new one (on shutdown, lost leadership, or a concurrent
// `login`) could leave the Secret holding a dead token.
func (r *FailoverIPReconciler) persistToken(ctx context.Context, name string, tokenJSON []byte) {
	log := logf.FromContext(ctx)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTokenTimeout)
	defer cancel()

	key := types.NamespacedName{Namespace: r.CredentialsNamespace, Name: name}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var secret corev1.Secret
		if err := r.Get(ctx, key, &secret); err != nil {
			return err
		}
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		secret.Data[netcup.TokenSecretKey] = tokenJSON
		return r.Update(ctx, &secret)
	})
	if err != nil {
		log.Error(err, "persisting refreshed token to credentials secret", "secret", name)
	}
}

// setCondition sets the Routed condition and writes the status.
func (r *FailoverIPReconciler) setCondition(
	ctx context.Context,
	fip *netcupv1alpha1.NetcupFailoverIP,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	if len(message) > maxConditionMessage {
		message = strings.ToValidUTF8(message[:maxConditionMessage], "") + "…"
	}
	meta.SetStatusCondition(&fip.Status.Conditions, metav1.Condition{
		Type:               netcupv1alpha1.ConditionRouted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: fip.Generation,
	})
	if err := r.Status().Update(ctx, fip); err != nil {
		return fmt.Errorf("updating status of %s: %w", fip.Name, err)
	}
	return nil
}

// unroutable records an outcome that retrying cannot fix: it resolves only
// when the spec or the nodes change, and both trigger a new reconcile. No
// error is returned for these, so this is the only place they are logged.
func (r *FailoverIPReconciler) unroutable(
	ctx context.Context,
	fip *netcupv1alpha1.NetcupFailoverIP,
	reason, message string,
) error {
	logf.FromContext(ctx).Info("cannot route failover IPs", "reason", reason, "detail", message)
	return r.setCondition(ctx, fip, metav1.ConditionFalse, reason, message)
}

// fail records a retryable failure on the Routed condition and returns err so
// the reconcile is requeued with backoff.
//
// While only verifying an already routed group the condition is left alone:
// failing to ask netcup says nothing about where the IPs are routed, and the
// check is retried with backoff until it gets an answer.
func (r *FailoverIPReconciler) fail(
	ctx context.Context,
	fip *netcupv1alpha1.NetcupFailoverIP,
	verifying bool,
	reason string,
	err error,
) error {
	if verifying {
		return fmt.Errorf("verifying routing of %s: %w", fip.Name, err)
	}
	return errors.Join(err, r.setCondition(ctx, fip, metav1.ConditionFalse, reason, err.Error()))
}

// SetupWithManager registers the reconciler, watching NetcupFailoverIP
// resources and the Node events that can change routing decisions.
//
// Only spec changes (generation bumps) of a NetcupFailoverIP trigger a
// reconcile. Without the predicate the controller's own status writes would
// re-enqueue the resource immediately, bypassing the error backoff and
// spending the netcup routing rate limit on instant retries.
func (r *FailoverIPReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&netcupv1alpha1.NetcupFailoverIP{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
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
