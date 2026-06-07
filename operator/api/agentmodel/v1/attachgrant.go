package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// AttachGrant is the K8s-native wrapper around the pure AttachGrantSpec — the
// durable authorization record for a human attaching to an agent's interactive
// terminal (M4.6, D5). cmd/agentterminal resolves a live, unexpired grant before
// minting an audience-bound attach token.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ag
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef`
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject`
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.role`
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.spec.expiresAt`
type AttachGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.AttachGrantSpec   `json:"spec"`
	Status pure.AttachGrantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AttachGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AttachGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AttachGrant{}, &AttachGrantList{})
}
