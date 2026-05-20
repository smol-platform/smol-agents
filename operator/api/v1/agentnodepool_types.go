package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AgentNodePoolSpec declares a kata-capable node shape. The operator
// compiles each AgentNodePool into a Karpenter NodePool + EC2NodeClass
// (dedicated, tainted) and the workload builder binds agents whose
// sandbox runtimeClass matches `isolation` onto it. Node→cluster join
// is supplied by the existing Karpenter deployment; this CR only adds
// the kata/devmapper layer. See docs/design/agent-platform.md.
//
// Implements R-PROV-1, R-PROV-3, R-PROV-4.
type AgentNodePoolSpec struct {
	// Isolation this pool provides. Drives the KVM/metal requirement and
	// the runtimeClass→pool match performed by the workload builder.
	// +kubebuilder:validation:Enum=kata-fc;kata-clh;gvisor;runc
	Isolation string `json:"isolation"`

	// Provider selects the node-provisioning backend (R-PROV-3):
	//   - Karpenter (default): the operator creates in-cluster
	//     NodePool + EC2NodeClass objects.
	//   - ClusterAutoscaler: CAS scales an externally-managed cloud ASG;
	//     the operator can't create the node group, so it emits the
	//     node-group spec (CAS discovery tags + kata userData) as a
	//     ConfigMap for the cluster's IaC to apply. The workload coupling
	//     (pool label + isolation taint) is identical for both.
	// +kubebuilder:validation:Enum=Karpenter;ClusterAutoscaler
	// +kubebuilder:default:=Karpenter
	Provider string `json:"provider,omitempty"`

	// Arch constrains the node architecture.
	// +kubebuilder:validation:Enum=arm64;amd64
	// +kubebuilder:default:=arm64
	Arch string `json:"arch,omitempty"`

	// InstanceFamilies Karpenter may choose from. For kata isolations these
	// MUST be bare-metal families — only `*.metal` instances expose /dev/kvm.
	InstanceFamilies []string `json:"instanceFamilies,omitempty"`

	// CapacityType is the Karpenter capacity-type allow-list.
	// +kubebuilder:default:={"on-demand"}
	CapacityType []string `json:"capacityType,omitempty"`

	// MinNodes keeps a warm floor (0 = pure on-demand provisioning).
	// Reserved for P2 serverless cold-start mitigation.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default:=0
	MinNodes int32 `json:"minNodes,omitempty"`

	// Limits caps total resources Karpenter will provision for this pool.
	Limits ResourceLimits `json:"limits,omitempty"`

	// Bootstrap selects how the kata layer is delivered to the node.
	Bootstrap NodeBootstrap `json:"bootstrap"`

	// ThinPool configures the devmapper thin-pool kata-fc needs.
	ThinPool ThinPoolConfig `json:"thinPool,omitempty"`

	// Disruption maps to the Karpenter NodePool disruption policy.
	Disruption DisruptionConfig `json:"disruption,omitempty"`
}

// NodeBootstrap selects the kata-layer delivery mechanism. Both ride on
// top of the existing Karpenter node-join (join + providerID are external).
type NodeBootstrap struct {
	// Mode is PrebakedAMI (kata baked onto the join-capable base image;
	// userData only creates the thin-pool) or UserData (kata recipe
	// appended after the existing join snippet, installed at boot).
	// +kubebuilder:validation:Enum=PrebakedAMI;UserData
	Mode string `json:"mode"`

	// AMISelector selects the kata-ready image for Mode=PrebakedAMI.
	AMISelector []AMISelectorTerm `json:"amiSelector,omitempty"`

	// Distro picks which hardened recipe to append for Mode=UserData.
	// +kubebuilder:validation:Enum=al2023;bottlerocket;flatcar;fedora-coreos
	Distro string `json:"distro,omitempty"`
}

// AMISelectorTerm mirrors a Karpenter EC2NodeClass amiSelectorTerm.
type AMISelectorTerm struct {
	Tags  map[string]string `json:"tags,omitempty"`
	ID    string            `json:"id,omitempty"`
	Alias string            `json:"alias,omitempty"`
}

// ThinPoolConfig sizes the devmapper thin-pool. For metal nodes the pool
// is built on raw instance-store NVMe at firstboot (ephemeral per launch).
type ThinPoolConfig struct {
	// +kubebuilder:validation:Enum=instance-store;ebs
	// +kubebuilder:default:=instance-store
	Backing string `json:"backing,omitempty"`

	// +kubebuilder:default:="50Gi"
	DataSize string `json:"dataSize,omitempty"`

	// +kubebuilder:default:="5Gi"
	MetaSize string `json:"metaSize,omitempty"`
}

// ResourceLimits caps the pool's aggregate provisioned resources.
type ResourceLimits struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// DisruptionConfig maps to Karpenter NodePool.spec.disruption. Defaults
// are conservative because live agent microVMs must not be consolidated
// out from under running work (R-PROV-5; pods also carry do-not-disrupt).
type DisruptionConfig struct {
	// +kubebuilder:validation:Enum=WhenEmpty;WhenEmptyOrUnderutilized
	// +kubebuilder:default:=WhenEmpty
	ConsolidationPolicy string `json:"consolidationPolicy,omitempty"`

	// ConsolidateAfter is a Karpenter duration string or "Never".
	// +kubebuilder:default:="Never"
	ConsolidateAfter string `json:"consolidateAfter,omitempty"`
}

// AgentNodePoolStatus is the observed state.
type AgentNodePoolStatus struct {
	// ObservedGeneration is the last generation reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is one of Pending, Reconciling, Ready, Degraded.
	// +kubebuilder:validation:Enum=Pending;Reconciling;Ready;Degraded
	Phase string `json:"phase,omitempty"`

	// Conditions carries the aggregate "Ready" and "KarpenterSynced".
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// NodePoolName / NodeClassName are the owned Karpenter object names.
	NodePoolName  string `json:"nodePoolName,omitempty"`
	NodeClassName string `json:"nodeClassName,omitempty"`

	// CapacityAvailable mirrors whether Karpenter reports launchable capacity.
	CapacityAvailable bool `json:"capacityAvailable,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=anp
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Isolation",type=string,JSONPath=`.spec.isolation`
// +kubebuilder:printcolumn:name="Arch",type=string,JSONPath=`.spec.arch`
// +kubebuilder:printcolumn:name="Bootstrap",type=string,JSONPath=`.spec.bootstrap.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Capacity",type=boolean,JSONPath=`.status.capacityAvailable`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentNodePool is the cluster-scoped node-shape CR.
type AgentNodePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentNodePoolSpec   `json:"spec,omitempty"`
	Status AgentNodePoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentNodePoolList is a list of AgentNodePool objects.
type AgentNodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentNodePool `json:"items"`
}

// Hand-rolled DeepCopy — follows the SmolAgentPlatform precedent in
// this package (these cluster-scoped types are not in zz_generated).

func (in *AgentNodePool) DeepCopyInto(out *AgentNodePool) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}
func (in *AgentNodePool) DeepCopy() *AgentNodePool {
	if in == nil {
		return nil
	}
	out := new(AgentNodePool)
	in.DeepCopyInto(out)
	return out
}
func (in *AgentNodePool) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *AgentNodePoolList) DeepCopyInto(out *AgentNodePoolList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AgentNodePool, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
func (in *AgentNodePoolList) DeepCopy() *AgentNodePoolList {
	if in == nil {
		return nil
	}
	out := new(AgentNodePoolList)
	in.DeepCopyInto(out)
	return out
}
func (in *AgentNodePoolList) DeepCopyObject() runtime.Object { return in.DeepCopy() }

func (in *AgentNodePoolSpec) DeepCopyInto(out *AgentNodePoolSpec) {
	*out = *in
	if in.InstanceFamilies != nil {
		out.InstanceFamilies = append([]string(nil), in.InstanceFamilies...)
	}
	if in.CapacityType != nil {
		out.CapacityType = append([]string(nil), in.CapacityType...)
	}
	out.Limits = in.Limits
	in.Bootstrap.DeepCopyInto(&out.Bootstrap)
	out.ThinPool = in.ThinPool
	out.Disruption = in.Disruption
}

func (in *NodeBootstrap) DeepCopyInto(out *NodeBootstrap) {
	*out = *in
	if in.AMISelector != nil {
		out.AMISelector = make([]AMISelectorTerm, len(in.AMISelector))
		for i := range in.AMISelector {
			in.AMISelector[i].DeepCopyInto(&out.AMISelector[i])
		}
	}
}

func (in *AMISelectorTerm) DeepCopyInto(out *AMISelectorTerm) {
	*out = *in
	if in.Tags != nil {
		out.Tags = make(map[string]string, len(in.Tags))
		for k, v := range in.Tags {
			out.Tags[k] = v
		}
	}
}

func (in *AgentNodePoolStatus) DeepCopyInto(out *AgentNodePoolStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func init() {
	SchemeBuilder.Register(&AgentNodePool{}, &AgentNodePoolList{})
}
