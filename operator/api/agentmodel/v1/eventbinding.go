package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// EventBinding is the K8s-native wrapper around the pure EventBindingSpec — the
// platform's namespaced analog of a Knative Trigger (docs/design/event-intake.md,
// epic t0d). It routes a matched CloudEvent (delivered to the agentgateway's
// CloudEvents ingress) to a same-namespace work target: an Agent (→ AgentRun), an
// AgentTeam (→ per-event coordinator), an AgentSession (→ a turn), or an
// AgentWorkflow (→ a workflow run). Same-namespace only (D1).
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=eb
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.kind`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.filter.type`,priority=1
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EventBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.EventBindingSpec   `json:"spec"`
	Status pure.EventBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type EventBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EventBinding `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EventBinding{}, &EventBindingList{})
}
