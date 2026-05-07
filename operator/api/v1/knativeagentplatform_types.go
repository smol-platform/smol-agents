package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// KnativeAgentPlatformSpec carries cluster-wide defaults and the
// feature allow/deny matrix. Exactly one Platform CR is permitted per
// cluster — enforced by the validating webhook (R-OP-API-2).
type KnativeAgentPlatformSpec struct {
	// DefaultTrustDomain is used by tenant CRs that omit `spec.trustDomain`.
	// +kubebuilder:validation:MinLength=1
	DefaultTrustDomain string `json:"defaultTrustDomain"`

	// Defaults are merged into every tenant CR before validation.
	Defaults Features `json:"defaults,omitempty"`

	// FeaturePolicy is consulted by the validating webhook; tenant CRs
	// enabling a feature with `allowed: false` are rejected.
	// +optional
	FeaturePolicy []FeaturePolicyRow `json:"featurePolicy,omitempty"`

	// EBPFLoader configures the per-cluster ebpf-loader DaemonSet.
	EBPFLoader EBPFLoaderSpec `json:"ebpfLoader,omitempty"`

	// Canary controls the percentage of tenant CRs that pick up new
	// non-Immediate rollouts.
	Canary CanaryConfig `json:"canary,omitempty"`
}

// EBPFLoaderSpec mirrors the chart's ebpfLoader values block.
type EBPFLoaderSpec struct {
	// +kubebuilder:default:=true
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:validation:Enum=generic;gke-cos;eks-bottlerocket;aks-mariner;k3s;openshift;talos
	// +kubebuilder:default:=generic
	Preset string `json:"preset,omitempty"`

	Image string `json:"image,omitempty"`

	// +kubebuilder:default:="/sys/fs/bpf/knative-agents"
	PinRoot string `json:"pinRoot,omitempty"`

	// +kubebuilder:validation:Enum=privileged;minimal
	// +kubebuilder:default:=privileged
	CapabilityMode string `json:"capabilityMode,omitempty"`
}

// CanaryConfig drives Canary rollout policy.
type CanaryConfig struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default:=0
	Percent int32 `json:"percent,omitempty"`
}

// KnativeAgentPlatformStatus reports operator health at cluster scope.
type KnativeAgentPlatformStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	ManagedTenants     int32              `json:"managedTenants,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=knap
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="TrustDomain",type=string,JSONPath=`.spec.defaultTrustDomain`
// +kubebuilder:printcolumn:name="Loader",type=string,JSONPath=`.spec.ebpfLoader.preset`
// +kubebuilder:printcolumn:name="Canary",type=integer,JSONPath=`.spec.canary.percent`
// +kubebuilder:printcolumn:name="Tenants",type=integer,JSONPath=`.status.managedTenants`

// KnativeAgentPlatform is the cluster-scoped platform CR.
type KnativeAgentPlatform struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KnativeAgentPlatformSpec   `json:"spec,omitempty"`
	Status KnativeAgentPlatformStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// KnativeAgentPlatformList is a list of KnativeAgentPlatform objects.
type KnativeAgentPlatformList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnativeAgentPlatform `json:"items"`
}

// Hand-rolled DeepCopy

func (in *KnativeAgentPlatform) DeepCopyInto(out *KnativeAgentPlatform) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}
func (in *KnativeAgentPlatform) DeepCopy() *KnativeAgentPlatform {
	if in == nil {
		return nil
	}
	out := new(KnativeAgentPlatform)
	in.DeepCopyInto(out)
	return out
}
func (in *KnativeAgentPlatform) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *KnativeAgentPlatformList) DeepCopyInto(out *KnativeAgentPlatformList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]KnativeAgentPlatform, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
func (in *KnativeAgentPlatformList) DeepCopy() *KnativeAgentPlatformList {
	if in == nil {
		return nil
	}
	out := new(KnativeAgentPlatformList)
	in.DeepCopyInto(out)
	return out
}
func (in *KnativeAgentPlatformList) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *KnativeAgentPlatformSpec) DeepCopyInto(out *KnativeAgentPlatformSpec) {
	*out = *in
	in.Defaults.DeepCopyInto(&out.Defaults)
	if in.FeaturePolicy != nil {
		out.FeaturePolicy = append([]FeaturePolicyRow(nil), in.FeaturePolicy...)
	}
}

func (in *KnativeAgentPlatformStatus) DeepCopyInto(out *KnativeAgentPlatformStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func init() {
	SchemeBuilder.Register(&KnativeAgentPlatform{}, &KnativeAgentPlatformList{})
}
