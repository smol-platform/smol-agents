package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// AgentTeam is the K8s-native wrapper around the pure AgentTeamSpec — the durable
// record + governance envelope for a team of collaborating agents (multi-agent
// orchestration). P0 is the governed envelope: the operator validates the team,
// rolls member usage up field-wise into status, and is the OwnerReference GC root
// for the team's members.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=at
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Lead",type=string,JSONPath=`.spec.lead`
// +kubebuilder:printcolumn:name="Pattern",type=string,JSONPath=`.spec.pattern`
// +kubebuilder:printcolumn:name="Members",type=integer,JSONPath=`.status.members[*]`,priority=1
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AgentTeam struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.AgentTeamSpec   `json:"spec"`
	Status pure.AgentTeamStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentTeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentTeam `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentTeam{}, &AgentTeamList{})
}
