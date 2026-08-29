package controller

import (
	"context"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	netcupv1alpha1 "github.com/digilolnet/netcup-failover-controller/api/v1alpha1"
)

// eligibleNodes lists Ready nodes matching the resource's label selector (all
// Ready nodes when the selector is nil).
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

	eligible := []corev1.Node{}
	for _, node := range nodeList.Items {
		if isNodeReady(&node) {
			eligible = append(eligible, node)
		}
	}
	return eligible, nil
}

// occupiedNodes reports which nodes already host another failover IP group,
// so selectNode can spread groups for bandwidth splitting.
func (r *FailoverIPReconciler) occupiedNodes(ctx context.Context, current *netcupv1alpha1.NetcupFailoverIP) (map[string]bool, error) {
	var list netcupv1alpha1.NetcupFailoverIPList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	occupied := map[string]bool{}
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

// nodesInAccount filters nodes to those whose server-name annotation maps to
// a server in the session's netcup account — a group may only be placed where
// its credentials can route. Nodes without the annotation are excluded too.
func nodesInAccount(nodes []corev1.Node, serverIDs map[string]int32) []corev1.Node {
	usable := []corev1.Node{}
	for _, n := range nodes {
		if _, ok := serverIDs[n.Annotations[annotationServerName]]; ok {
			usable = append(usable, n)
		}
	}
	return usable
}

// selectNode picks a node deterministically, preferring nodes not already
// hosting another failover IP group. The input slice is not modified.
func selectNode(name string, nodes []corev1.Node, occupied map[string]bool) corev1.Node {
	sorted := slices.Clone(nodes)
	slices.SortFunc(sorted, func(a, b corev1.Node) int { return strings.Compare(a.Name, b.Name) })

	free := []corev1.Node{}
	for _, n := range sorted {
		if !occupied[n.Name] {
			free = append(free, n)
		}
	}
	pool := sorted
	if len(free) > 0 {
		pool = free
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return pool[h.Sum32()%uint32(len(pool))] // #nosec G115 -- node counts are far below MaxInt32
}

// nodeByName returns the named node, or a zero Node when absent.
func nodeByName(nodes []corev1.Node, name string) corev1.Node {
	if name == "" {
		return corev1.Node{}
	}
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return corev1.Node{}
}

func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// nodePredicate filters out node updates that cannot change routing decisions,
// such as heartbeats and image list churn. Readiness, labels (selectors), and
// annotations (server name) matter.
func nodePredicate() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok1 := e.ObjectOld.(*corev1.Node)
			newNode, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return false
			}
			return isNodeReady(oldNode) != isNodeReady(newNode) ||
				!maps.Equal(oldNode.Labels, newNode.Labels) ||
				!maps.Equal(oldNode.Annotations, newNode.Annotations)
		},
	}
}
