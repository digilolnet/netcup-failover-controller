package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ConditionRouted is the single condition type reported on a
// NetcupFailoverIP: True once every spec IP is routed to CurrentNode.
const ConditionRouted = "Routed"

// Reasons for the Routed condition.
const (
	// ReasonNodeSelected: routing is complete.
	ReasonNodeSelected = "NodeSelected"
	// ReasonRouting: a target node was selected and routing is in progress.
	ReasonRouting = "Routing"
	// ReasonRateLimited: waiting out the netcup routing rate limit.
	ReasonRateLimited = "RateLimited"
	// ReasonRoutingFailed: a routing call or its async task failed.
	ReasonRoutingFailed = "RoutingFailed"
	// ReasonCredentialsError: the credentials Secret is missing or invalid.
	ReasonCredentialsError = "CredentialsError"
	// ReasonNoEligibleNodes: no healthy node matches the selector.
	ReasonNoEligibleNodes = "NoEligibleNodes"
	// ReasonMissingAnnotation: the selected node has no server-name annotation.
	ReasonMissingAnnotation = "MissingAnnotation"
	// ReasonInvalidIP: a spec entry is not a valid CIDR.
	ReasonInvalidIP = "InvalidIP"
	// ReasonServerNotFound: the annotated server name is not in the account.
	ReasonServerNotFound = "ServerNotFound"
	// ReasonServerLookupError: resolving the server name failed transiently.
	ReasonServerLookupError = "ServerLookupError"
)

type NetcupFailoverIPSpec struct {
	// IPs is the list of failover IPs in CIDR notation to route together to the same node (e.g. 198.51.100.1/32 or 2001:db8::/64).
	IPs []string `json:"ips"`
	// CredentialsSecret names the Secret containing the netcup SCP API
	// credentials. The Secret always lives in the controller's own namespace
	// (its RBAC on Secrets is restricted to it) and must have a "token" key
	// holding the OAuth2 token JSON; seed it with
	// `netcup-failover-controller login`.
	CredentialsSecret corev1.LocalObjectReference `json:"credentialsSecret"`
	// NodeSelector restricts the set of eligible nodes. Supports the full
	// Kubernetes label selector syntax (matchLabels and matchExpressions).
	// If omitted, all ready nodes are eligible.
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
}

type NetcupFailoverIPStatus struct {
	// CurrentNode is the node the IPs are routed to. While the Routed
	// condition is False it names the node currently being routed to.
	CurrentNode string             `json:"currentNode,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
}

// NetcupFailoverIP is a group of netcup failover IPs that are routed together
// to a single healthy Kubernetes node. The controller selects the node
// deterministically and spreads groups across nodes for bandwidth splitting.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".status.currentNode"
// +kubebuilder:printcolumn:name="Routed",type=string,JSONPath=".status.conditions[?(@.type==\"Routed\")].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type NetcupFailoverIP struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NetcupFailoverIPSpec   `json:"spec"`
	Status            NetcupFailoverIPStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type NetcupFailoverIPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetcupFailoverIP `json:"items"`
}

// DeepCopy methods — required to satisfy runtime.Object.

func (in *NetcupFailoverIP) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *NetcupFailoverIP) DeepCopy() *NetcupFailoverIP {
	if in == nil {
		return nil
	}
	out := new(NetcupFailoverIP)
	in.DeepCopyInto(out)
	return out
}

func (in *NetcupFailoverIP) DeepCopyInto(out *NetcupFailoverIP) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *NetcupFailoverIPSpec) DeepCopyInto(out *NetcupFailoverIPSpec) {
	*out = *in
	if in.IPs != nil {
		out.IPs = make([]string, len(in.IPs))
		copy(out.IPs, in.IPs)
	}
	if in.NodeSelector != nil {
		out.NodeSelector = in.NodeSelector.DeepCopy()
	}
}

func (in *NetcupFailoverIPStatus) DeepCopyInto(out *NetcupFailoverIPStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		copy(out.Conditions, in.Conditions)
	}
}

func (in *NetcupFailoverIPList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

func (in *NetcupFailoverIPList) DeepCopy() *NetcupFailoverIPList {
	if in == nil {
		return nil
	}
	out := new(NetcupFailoverIPList)
	in.DeepCopyInto(out)
	return out
}

func (in *NetcupFailoverIPList) DeepCopyInto(out *NetcupFailoverIPList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]NetcupFailoverIP, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
