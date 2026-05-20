package builders

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1 "github.com/stigen/smol-agents/operator/api/v1"
)

// Karpenter object rendering for AgentNodePool. We emit Karpenter v1
// objects as unstructured so the operator carries no Karpenter Go
// dependency (CNCF-flexible; see docs/design/agent-platform.md). Node→k0s
// join is owned by the existing Karpenter deployment — these builders only
// add the kata/devmapper layer and compose onto the provided join snippet.
const (
	// PoolLabelKey ties a provisioned node (and the agents scheduled onto
	// it) back to its AgentNodePool. The workload builder uses it for
	// nodeAffinity.
	PoolLabelKey = "agents.stigen.ai/pool"
	// IsolationTaintKey keeps general workloads off dedicated agent nodes;
	// the workload builder adds the matching toleration for sandboxed agents.
	IsolationTaintKey = "agents.stigen.ai/isolation"

	karpenterManagedBy = "smol-agents-operator"
)

var (
	nodePoolGVK     = schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodePool"}
	ec2NodeClassGVK = schema.GroupVersionKind{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass"}
)

// KarpenterDefaults are cluster-level inputs the operator sources from the
// Platform CR's nodeProvisioning block (TODO(P1): wire once that field
// exists). Modelled here so the builders stay pure and unit-testable.
type KarpenterDefaults struct {
	// AMIFamily for the EC2NodeClass. "Custom" on k0s (we own all userData).
	AMIFamily string
	// Role is the EC2NodeClass node IAM role.
	Role string
	// SubnetSelectorTags / SecurityGroupSelectorTags drive Karpenter's
	// subnet + security-group discovery.
	SubnetSelectorTags        map[string]string
	SecurityGroupSelectorTags map[string]string
	// BaseAMISelector is the existing join-capable image, used when
	// Bootstrap.Mode == UserData (kata is appended at boot).
	BaseAMISelector []v1.AMISelectorTerm
	// JoinUserData is the existing Karpenter deployment's node-join snippet
	// (k0s worker-join + kubelet providerID). The kata layer is appended to
	// it; the operator never owns join. Empty if the base AMI self-joins.
	JoinUserData string
}

// KarpenterNames returns the deterministic names of the owned objects. The
// "anp-" prefix marks them operator-owned and avoids colliding with the
// existing deployment's own NodePools/EC2NodeClasses.
func KarpenterNames(anp *v1.AgentNodePool) (nodePool, nodeClass string) {
	return "anp-" + anp.Name, "anp-" + anp.Name
}

// RequiresKVM reports whether an isolation needs /dev/kvm (hence a
// bare-metal node). Only the kata variants run a real microVM.
func RequiresKVM(isolation string) bool {
	return strings.HasPrefix(isolation, "kata")
}

// BuildKarpenterNodePool renders the kata-dedicated NodePool for anp.
func BuildKarpenterNodePool(anp *v1.AgentNodePool, nodeClassName string) *unstructured.Unstructured {
	reqs := []interface{}{
		requirement("kubernetes.io/arch", "In", []string{anp.Spec.Arch}),
		requirement("karpenter.sh/capacity-type", "In", anp.Spec.CapacityType),
	}
	if len(anp.Spec.InstanceFamilies) > 0 {
		reqs = append(reqs, requirement("karpenter.k8s.aws/instance-family", "In", anp.Spec.InstanceFamilies))
	}
	if RequiresKVM(anp.Spec.Isolation) {
		// metal size is what exposes /dev/kvm on AWS.
		reqs = append(reqs, requirement("karpenter.k8s.aws/instance-size", "In", []string{"metal"}))
	}

	disruption := map[string]interface{}{
		"consolidationPolicy": orDefault(anp.Spec.Disruption.ConsolidationPolicy, "WhenEmpty"),
		"consolidateAfter":    orDefault(anp.Spec.Disruption.ConsolidateAfter, "Never"),
	}

	templateSpec := map[string]interface{}{
		"requirements": reqs,
		"taints": []interface{}{
			map[string]interface{}{
				"key":    IsolationTaintKey,
				"value":  anp.Spec.Isolation,
				"effect": "NoSchedule",
			},
		},
		"nodeClassRef": map[string]interface{}{
			"group": ec2NodeClassGVK.Group,
			"kind":  ec2NodeClassGVK.Kind,
			"name":  nodeClassName,
		},
		"expireAfter": "720h",
	}

	spec := map[string]interface{}{
		"template": map[string]interface{}{
			"metadata": map[string]interface{}{
				"labels": map[string]interface{}{PoolLabelKey: anp.Name},
			},
			"spec": templateSpec,
		},
		"disruption": disruption,
	}
	if limits := resourceLimits(anp.Spec.Limits); len(limits) > 0 {
		spec["limits"] = limits
	}

	nodePoolName, _ := KarpenterNames(anp)
	return newKarpenterObject(nodePoolGVK, nodePoolName, anp.Name, spec)
}

// BuildKarpenterEC2NodeClass renders the EC2NodeClass for anp, composing the
// kata layer onto the existing join (defaults.JoinUserData / BaseAMISelector).
func BuildKarpenterEC2NodeClass(anp *v1.AgentNodePool, defaults KarpenterDefaults) *unstructured.Unstructured {
	amiTerms := defaults.BaseAMISelector
	if anp.Spec.Bootstrap.Mode == "PrebakedAMI" {
		amiTerms = anp.Spec.Bootstrap.AMISelector
	}

	spec := map[string]interface{}{
		"amiFamily":                  orDefault(defaults.AMIFamily, "Custom"),
		"amiSelectorTerms":           amiSelectorTerms(amiTerms),
		"subnetSelectorTerms":        tagSelectorTerms(defaults.SubnetSelectorTags),
		"securityGroupSelectorTerms": tagSelectorTerms(defaults.SecurityGroupSelectorTags),
		"userData":                   composeUserData(anp, defaults),
	}
	if defaults.Role != "" {
		spec["role"] = defaults.Role
	}

	_, nodeClassName := KarpenterNames(anp)
	return newKarpenterObject(ec2NodeClassGVK, nodeClassName, anp.Name, spec)
}

// composeUserData appends the kata layer (BuildKataLayer) after the
// existing node-join snippet. For PrebakedAMI the kata binaries are already
// baked, so only the per-launch thin-pool + drop-ins are emitted.
func composeUserData(anp *v1.AgentNodePool, defaults KarpenterDefaults) string {
	var b strings.Builder
	if defaults.JoinUserData != "" {
		b.WriteString(defaults.JoinUserData)
		b.WriteString("\n")
	}
	installKata := anp.Spec.Bootstrap.Mode != "PrebakedAMI"
	b.WriteString(BuildKataLayer(
		orDefault(anp.Spec.Bootstrap.Distro, "al2023"),
		orDefault(anp.Spec.Arch, "arm64"),
		anp.Spec.ThinPool,
		installKata,
	))
	return b.String()
}

// --- helpers -------------------------------------------------------------

func newKarpenterObject(gvk schema.GroupVersionKind, name, pool string, spec map[string]interface{}) *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]interface{}{}}
	o.SetGroupVersionKind(gvk)
	o.SetName(name)
	o.SetLabels(map[string]string{
		"app.kubernetes.io/managed-by": karpenterManagedBy,
		PoolLabelKey:                   pool,
	})
	o.Object["spec"] = spec
	return o
}

func requirement(key, op string, values []string) map[string]interface{} {
	return map[string]interface{}{
		"key":      key,
		"operator": op,
		"values":   toIface(values),
	}
}

func amiSelectorTerms(terms []v1.AMISelectorTerm) []interface{} {
	out := make([]interface{}, 0, len(terms))
	for _, t := range terms {
		m := map[string]interface{}{}
		if t.ID != "" {
			m["id"] = t.ID
		}
		if t.Alias != "" {
			m["alias"] = t.Alias
		}
		if len(t.Tags) > 0 {
			m["tags"] = toIfaceMap(t.Tags)
		}
		out = append(out, m)
	}
	return out
}

func tagSelectorTerms(tags map[string]string) []interface{} {
	if len(tags) == 0 {
		return []interface{}{}
	}
	return []interface{}{map[string]interface{}{"tags": toIfaceMap(tags)}}
}

func resourceLimits(l v1.ResourceLimits) map[string]interface{} {
	out := map[string]interface{}{}
	if l.CPU != "" {
		out["cpu"] = l.CPU
	}
	if l.Memory != "" {
		out["memory"] = l.Memory
	}
	return out
}

func toIface(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func toIfaceMap(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
