package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// ModelGateway is the K8s-native wrapper around ModelGatewaySpec (yxh.2): an
// operator-managed model/agent gateway. The reconciler renders + hardens its
// Deployment, Service, config ConfigMap, and egress/ingress NetworkPolicies, so
// the gateway is declared as a CR instead of a manual workload. Namespaced.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=mgw
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ModelGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.ModelGatewaySpec   `json:"spec"`
	Status pure.ModelGatewayStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ModelGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelGateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelGateway{}, &ModelGatewayList{})
}
