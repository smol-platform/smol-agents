package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// AgentNetwork is the K8s-native wrapper around the pure
// AgentNetworkSpec. Implements R-AN-API-1.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=anet
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Resources",type=integer,JSONPath=`.status.proxyResourceCount`
// +kubebuilder:printcolumn:name="WG-Peers",type=integer,JSONPath=`.status.wgPeerCount`
// +kubebuilder:printcolumn:name="Bound",type=integer,JSONPath=`.status.boundAgents`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AgentNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.AgentNetworkSpec   `json:"spec"`
	Status pure.AgentNetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentNetwork{}, &AgentNetworkList{})
}
