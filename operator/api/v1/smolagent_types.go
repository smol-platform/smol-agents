package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SmolAgentSpec is the desired state of a tenant agent. Implements
// R-OP-API-1.
type SmolAgentSpec struct {
	// TrustDomain MUST match the SPIRE deployment's trust domain.
	// +kubebuilder:validation:MinLength=1
	TrustDomain string `json:"trustDomain"`

	// Mode is the legacy compatibility shorthand also accepted by the
	// chart: insecure | permissive | strict. If set it overrides
	// `features.identity.mode`.
	// +kubebuilder:validation:Enum=insecure;permissive;strict
	// +optional
	Mode string `json:"mode,omitempty"`

	// Deployment selects how the agent runs.
	// +kubebuilder:validation:Enum=knative;deployment;statefulset
	// +kubebuilder:default:=knative
	DeploymentKind string `json:"deploymentKind,omitempty"`

	// Image is the agent runtime image override.
	// +optional
	Image string `json:"image,omitempty"`

	// Replicas applies to deployment / statefulset.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=1
	Replicas int32 `json:"replicas,omitempty"`

	// Features carries per-capability config.
	Features Features `json:"features,omitempty"`

	// Rollout pauses or resumes operator-driven changes.
	Rollout RolloutPolicy `json:"rollout,omitempty"`
}

// RolloutPolicy lets operators pause a CR's reconcile.
type RolloutPolicy struct {
	// Paused freezes operator-driven changes; existing resources stay.
	Paused bool `json:"paused,omitempty"`
}

// FeatureStatus is the per-feature view recorded in Status.
type FeatureStatus struct {
	Enabled            bool        `json:"enabled"`
	Ready              bool        `json:"ready"`
	Mode               string      `json:"mode,omitempty"`
	Reason             string      `json:"reason,omitempty"`
	Message            string      `json:"message,omitempty"`
	LastTransitionTime metav1.Time `json:"lastTransitionTime,omitempty"`
}

// SmolAgentStatus is the observed state. R-OP-FF-3.
type SmolAgentStatus struct {
	// ObservedGeneration is the last generation we reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is one of Pending, Reconciling, Ready, Failed.
	// +kubebuilder:validation:Enum=Pending;Reconciling;Ready;Failed
	Phase string `json:"phase,omitempty"`

	// Conditions carries one entry per feature; aggregate "Ready" lives
	// at type=Ready.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// Features is keyed by feature constant; map keys are stable and
	// match operator/pkg/features.Feature.
	Features map[string]FeatureStatus `json:"features,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=kna;agent
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Identity",type=string,JSONPath=`.status.features.identity.ready`
// +kubebuilder:printcolumn:name="MTLS",type=string,JSONPath=`.status.features.transport\.private.ready`
// +kubebuilder:printcolumn:name="EBPF",type=string,JSONPath=`.status.features.ebpf.ready`
// +kubebuilder:printcolumn:name="Secrets",type=string,JSONPath=`.status.features.secrets.ready`
// +kubebuilder:printcolumn:name="RuntimeClass",type=string,JSONPath=`.spec.features.sandbox.runtimeClass`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SmolAgent is the namespaced top-level CR.
type SmolAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SmolAgentSpec   `json:"spec,omitempty"`
	Status SmolAgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SmolAgentList is a list of SmolAgent objects.
type SmolAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SmolAgent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SmolAgent{}, &SmolAgentList{})
}
