package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// AgentWorkflow is the K8s-native wrapper around the pure AgentWorkflowSpec — a
// declarative, result-routed DAG of agent steps (LangGraph StateGraph). The
// operator materializes each ready node as a child AgentRun and routes on the
// node's terminal output via the operator-evaluated predicate DSL.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=awf
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.spec.nodes[*]`,priority=1
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AgentWorkflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.AgentWorkflowSpec   `json:"spec"`
	Status pure.AgentWorkflowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentWorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentWorkflow `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentWorkflow{}, &AgentWorkflowList{})
}
