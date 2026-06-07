package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// DynamicCredentialBackend is the K8s-native wrapper around the pure
// DynamicCredentialBackendSpec (D8): platform-owned infrastructure that mints
// short-lived provider credentials from a broker-only root secret. Namespaced
// (recommend a RBAC-locked platform-secrets namespace).
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=dcb
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Credential",type=string,JSONPath=`.spec.credentialName`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Grants",type=integer,JSONPath=`.status.grantCount`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DynamicCredentialBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.DynamicCredentialBackendSpec   `json:"spec"`
	Status pure.DynamicCredentialBackendStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DynamicCredentialBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DynamicCredentialBackend `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DynamicCredentialBackend{}, &DynamicCredentialBackendList{})
}
