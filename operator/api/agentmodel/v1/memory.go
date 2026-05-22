package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// MemoryStore declares a memory backend (vector / graph / kv / eventlog /
// filesystem). The operator reconciles this into retrieval-worker infrastructure
// and wires backend credentials via the secret broker.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=mstore
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.spec.driver`
// +kubebuilder:printcolumn:name="Tenancy",type=string,JSONPath=`.spec.tenancy.model`
// +kubebuilder:printcolumn:name="Workers",type=integer,JSONPath=`.status.boundWorkers`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type MemoryStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.MemoryStoreSpec   `json:"spec"`
	Status pure.MemoryStoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MemoryStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryStore `json:"items"`
}

// MemoryRetriever declares a named retrieval pipeline that binds one or more
// MemoryStores, an embedding ModelProvider, access-control policy, and quota.
// The MCP server resolves a retrieverRef to this CR on each call.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=mret
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Stores",type=string,JSONPath=`.spec.stores`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.modelProviderRef`
// +kubebuilder:printcolumn:name="TopK",type=integer,JSONPath=`.spec.topK`
// +kubebuilder:printcolumn:name="Workers",type=integer,JSONPath=`.status.boundWorkers`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type MemoryRetriever struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   pure.MemoryRetrieverSpec   `json:"spec"`
	Status pure.MemoryRetrieverStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MemoryRetrieverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryRetriever `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&MemoryStore{}, &MemoryStoreList{},
		&MemoryRetriever{}, &MemoryRetrieverList{},
	)
}
