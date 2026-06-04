package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// AgentRunQuota is the K8s-native wrapper around the pure AgentRunQuotaSpec —
// the per-namespace concurrency cap the run reconciler enforces (D10).
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=arq
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.maxConcurrentRuns`
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activeRuns`
// +kubebuilder:printcolumn:name="Queued",type=integer,JSONPath=`.status.queuedRuns`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AgentRunQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.AgentRunQuotaSpec   `json:"spec"`
	Status pure.AgentRunQuotaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentRunQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRunQuota `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentRunQuota{}, &AgentRunQuotaList{})
}
