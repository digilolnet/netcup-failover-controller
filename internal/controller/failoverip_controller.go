// Package controller reconciles NetcupFailoverIP resources: it selects a
// healthy node per group and drives the netcup SCP API (via internal/netcup)
// until every failover IP is routed to that node's server.
package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
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

	// Event actions: what the controller was doing when the event occurred.
	actionRoute  = "Route"
	actionVerify = "Verify"
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
	// Recorder emits Events about a group: moved, drift corrected, rate
	// limited, failed. Optional; nil records nothing.
	Recorder events.EventRecorder
}

// Reconcile routes the group's failover IPs to a healthy eligible node. While
// the group is routed at the current generation and its node is still
// eligible it does nothing, except once per ResyncInterval, when it checks
// that netcup still routes the IPs there and re-routes them if not.
func (r *FailoverIPReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var fip netcupv1alpha1.NetcupFailoverIP
	if err := r.Get(ctx, req.NamespacedName, &fip); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
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
		if wait, due := r.resyncDue(&fip); !due {
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
			return ctrl.Result{}, r.fail(ctx, &fip, verifying, netcupv1alpha1.ReasonVerificationFailed, err)
		}
		if len(drifted) == 0 {
			return r.confirmed(ctx, &fip, node.Name)
		}
		log.Info("netcup no longer routes these failover IPs to the current node, re-routing",
			"ips", drifted, "node", node.Name, "server", serverName)
		r.event(&fip, corev1.EventTypeWarning, netcupv1alpha1.EventReasonDriftDetected, actionVerify,
			"netcup no longer routes %s to node %s; routing back", strings.Join(drifted, ", "), node.Name)
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
			r.event(&fip, corev1.EventTypeWarning, netcupv1alpha1.ReasonRateLimited, actionRoute,
				"netcup routing rate limit hit; retrying in %s", rateLimitRequeue)
			return ctrl.Result{RequeueAfter: rateLimitRequeue}, r.setCondition(ctx, &fip, metav1.ConditionFalse,
				netcupv1alpha1.ReasonRateLimited, "waiting out the netcup failover routing rate limit")
		}
		return ctrl.Result{}, r.fail(ctx, &fip, false, netcupv1alpha1.ReasonRoutingFailed, err)
	}

	log.Info("failover IPs routed", "node", node.Name, "server", serverName)
	r.event(&fip, corev1.EventTypeNormal, netcupv1alpha1.EventReasonRouted, actionRoute,
		"netcup routes %s to node %s", strings.Join(fip.Spec.IPs, ", "), node.Name)
	return r.confirmed(ctx, &fip, node.Name)
}

// resyncDue reports whether the group's routing should be checked against
// netcup now, and otherwise how long until it should. With the check disabled
// it is never due and nothing is requeued.
//
// The last check is read from status, not kept in memory, so it survives
// restarts and leader changes and is visible on the resource. A check that
// failed did not advance it, so it stays due and is retried with backoff.
func (r *FailoverIPReconciler) resyncDue(fip *netcupv1alpha1.NetcupFailoverIP) (wait time.Duration, due bool) {
	if r.ResyncInterval <= 0 {
		return 0, false
	}
	last := fip.Status.LastVerifiedTime
	if last == nil {
		return 0, true
	}
	wait = r.ResyncInterval - time.Since(last.Time)
	if wait <= 0 {
		return 0, true
	}
	// A timestamp from a leader whose clock ran ahead must not postpone the
	// check beyond one interval.
	return min(wait, r.ResyncInterval), false
}

// confirmed records that netcup routes the group to node as of now, and
// schedules the next drift check.
func (r *FailoverIPReconciler) confirmed(
	ctx context.Context,
	fip *netcupv1alpha1.NetcupFailoverIP,
	node string,
) (ctrl.Result, error) {
	fip.Status.LastVerifiedTime = new(metav1.Now())
	// The message says what was verified: netcup's routing, not that the node
	// answers on the addresses.
	r.condition(fip, netcupv1alpha1.ConditionRouted, metav1.ConditionTrue, netcupv1alpha1.ReasonNodeSelected,
		fmt.Sprintf("netcup routes the failover IPs to node %s", node))
	r.condition(fip, netcupv1alpha1.ConditionVerified, metav1.ConditionTrue, netcupv1alpha1.ReasonRoutingConfirmed,
		"netcup's routing matched at lastVerifiedTime")
	if err := r.writeStatus(ctx, fip); err != nil {
		return ctrl.Result{}, err
	}
	// Zero (check disabled) means no requeue.
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

// event records a Kubernetes Event about the group. Events on cluster-scoped
// objects are stored in the default namespace.
func (r *FailoverIPReconciler) event(
	fip *netcupv1alpha1.NetcupFailoverIP,
	eventType, reason, action, format string,
	args ...any,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(fip, nil, eventType, reason, action, "%s", truncate(fmt.Sprintf(format, args...)))
}

// truncate caps text destined for a condition message or an event note.
func truncate(message string) string {
	if len(message) > maxConditionMessage {
		return strings.ToValidUTF8(message[:maxConditionMessage], "") + "…"
	}
	return message
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

// condition sets one condition on the in-memory resource.
func (r *FailoverIPReconciler) condition(
	fip *netcupv1alpha1.NetcupFailoverIP,
	conditionType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	meta.SetStatusCondition(&fip.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            truncate(message),
		ObservedGeneration: fip.Generation,
	})
}

func (r *FailoverIPReconciler) writeStatus(ctx context.Context, fip *netcupv1alpha1.NetcupFailoverIP) error {
	if err := r.Status().Update(ctx, fip); err != nil {
		return fmt.Errorf("updating status of %s: %w", fip.Name, err)
	}
	return nil
}

// setCondition sets the Routed condition and writes the status. Verified
// describes a routed group, so it is dropped whenever Routed is not True.
func (r *FailoverIPReconciler) setCondition(
	ctx context.Context,
	fip *netcupv1alpha1.NetcupFailoverIP,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	r.condition(fip, netcupv1alpha1.ConditionRouted, status, reason, message)
	if status != metav1.ConditionTrue {
		meta.RemoveStatusCondition(&fip.Status.Conditions, netcupv1alpha1.ConditionVerified)
	}
	return r.writeStatus(ctx, fip)
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
	r.event(fip, corev1.EventTypeWarning, reason, actionRoute, "%s", message)
	return r.setCondition(ctx, fip, metav1.ConditionFalse, reason, message)
}

// fail records a retryable failure and returns err so the reconcile is
// requeued with backoff.
//
// While only verifying an already routed group, Routed is left alone: failing
// to ask netcup says nothing about where the IPs are routed. The failure goes
// on the Verified condition instead, so a long-broken token or API outage
// shows that Routed may be stale.
func (r *FailoverIPReconciler) fail(
	ctx context.Context,
	fip *netcupv1alpha1.NetcupFailoverIP,
	verifying bool,
	reason string,
	err error,
) error {
	if verifying {
		r.event(fip, corev1.EventTypeWarning, reason, actionVerify, "checking netcup's routing: %v", err)
		r.condition(fip, netcupv1alpha1.ConditionVerified, metav1.ConditionFalse, reason, err.Error())
		return errors.Join(fmt.Errorf("verifying routing of %s: %w", fip.Name, err), r.writeStatus(ctx, fip))
	}
	r.event(fip, corev1.EventTypeWarning, reason, actionRoute, "%v", err)
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
