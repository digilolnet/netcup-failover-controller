package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition types reported on a NetcupFailoverIP.
const (
	// ConditionRouted is True once netcup routes every spec IP to
	// CurrentNode. It says nothing about the node answering on the addresses.
	ConditionRouted = "Routed"
	// ConditionVerified reports the most recent comparison of the Routed
	// condition against netcup's actual routing: True if netcup could be
	// asked and agreed (or was corrected), False if it could not be asked, so
	// Routed may be stale. It is absent while Routed is not True.
	ConditionVerified = "Verified"
)

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
	// ReasonNoAccountNodes: eligible nodes exist, but none carries a
	// server-name annotation resolving to a server in the netcup account.
	ReasonNoAccountNodes = "NoAccountNodes"
	// ReasonInvalidIP: a spec entry is not a valid CIDR.
	ReasonInvalidIP = "InvalidIP"
	// ReasonInvalidNodeSelector: spec.nodeSelector is not a valid label selector.
	ReasonInvalidNodeSelector = "InvalidNodeSelector"
	// ReasonServerLookupError: listing the account's servers failed transiently.
	ReasonServerLookupError = "ServerLookupError"
)

// Reasons for the Verified condition. A failed check also uses
// ReasonCredentialsError and ReasonServerLookupError.
const (
	// ReasonRoutingConfirmed: netcup's routing matched the status, or was
	// just made to.
	ReasonRoutingConfirmed = "RoutingConfirmed"
	// ReasonVerificationFailed: listing the account's failover IPs failed.
	ReasonVerificationFailed = "VerificationFailed"
)

// Event reasons that are not also condition reasons.
const (
	// EventReasonRouted: the group was routed to a node.
	EventReasonRouted = "Routed"
	// EventReasonDriftDetected: netcup routed the IPs elsewhere than the
	// status said, and the controller is routing them back.
	EventReasonDriftDetected = "DriftDetected"
)

// NetcupFailoverIPSpec is the desired state of a NetcupFailoverIP.
type NetcupFailoverIPSpec struct {
	// IPs is the list of failover IPs in CIDR notation to route together to the same node (e.g. 198.51.100.1/32 or 2001:db8::/64).
	//
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	IPs []string `json:"ips"`
	// CredentialsSecret names the Secret containing the netcup SCP API
	// credentials. The Secret always lives in the controller's own namespace
	// (its RBAC on Secrets is restricted to it) and must have a "token" key
	// holding the OAuth2 token JSON; seed it with
	// `netcup-failover-controller login`.
	//
	// +required
	CredentialsSecret corev1.LocalObjectReference `json:"credentialsSecret"`
	// NodeSelector restricts the set of eligible nodes. Supports the full
	// Kubernetes label selector syntax (matchLabels and matchExpressions).
	// If omitted, all ready nodes are eligible.
	//
	// +optional
	NodeSelector *metav1.LabelSelector `json:"nodeSelector,omitempty"`
}

// NetcupFailoverIPStatus is the observed state of a NetcupFailoverIP.
type NetcupFailoverIPStatus struct {
	// CurrentNode is the node the IPs are routed to. While the Routed
	// condition is False it names the node currently being routed to.
	//
	// +optional
	CurrentNode string `json:"currentNode,omitempty"`
	// LastVerifiedTime is when netcup's routing was last confirmed to match
	// CurrentNode, either by routing there or by the periodic drift check.
	//
	// +optional
	LastVerifiedTime *metav1.Time `json:"lastVerifiedTime,omitempty"`
	// Conditions holds the Routed and Verified conditions (see
	// ConditionRouted and ConditionVerified).
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
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
// +kubebuilder:printcolumn:name="Verified",type=date,JSONPath=".status.lastVerifiedTime",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type NetcupFailoverIP struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NetcupFailoverIPSpec   `json:"spec"`
	Status            NetcupFailoverIPStatus `json:"status,omitempty"`
}

// NetcupFailoverIPList is a list of NetcupFailoverIP resources.
//
// +kubebuilder:object:root=true
type NetcupFailoverIPList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetcupFailoverIP `json:"items"`
}

// The DeepCopy methods below are handwritten (the project does not use
// controller-gen); keep them in sync with the types above.

// DeepCopyObject implements runtime.Object.
func (in *NetcupFailoverIP) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

// DeepCopy returns a deep copy of the resource.
func (in *NetcupFailoverIP) DeepCopy() *NetcupFailoverIP {
	if in == nil {
		return nil
	}
	out := new(NetcupFailoverIP)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the resource into out.
func (in *NetcupFailoverIP) DeepCopyInto(out *NetcupFailoverIP) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopyInto copies the spec into out.
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

// DeepCopyInto copies the status into out.
func (in *NetcupFailoverIPStatus) DeepCopyInto(out *NetcupFailoverIPStatus) {
	*out = *in
	if in.LastVerifiedTime != nil {
		out.LastVerifiedTime = in.LastVerifiedTime.DeepCopy()
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

// DeepCopyObject implements runtime.Object.
func (in *NetcupFailoverIPList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

// DeepCopy returns a deep copy of the list.
func (in *NetcupFailoverIPList) DeepCopy() *NetcupFailoverIPList {
	if in == nil {
		return nil
	}
	out := new(NetcupFailoverIPList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the list into out.
func (in *NetcupFailoverIPList) DeepCopyInto(out *NetcupFailoverIPList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]NetcupFailoverIP, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
