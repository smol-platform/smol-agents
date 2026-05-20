package builders

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/stigen/knative-agents/operator/api/v1"
)

// Cluster Autoscaler provider. Unlike Karpenter (which the operator drives
// via in-cluster CRDs), CAS scales pre-existing cloud ASGs that live outside
// Kubernetes — the operator cannot create the node group. So for a
// ClusterAutoscaler AgentNodePool the operator emits the *node-group spec*
// the ASG/launch-template must satisfy (CAS discovery tags, the pool label +
// isolation taint the coupling targets, instance families, and the kata
// userData) as a ConfigMap for the cluster's IaC to apply. The workload
// coupling is identical to Karpenter, so CAS sees the pending kata pods and
// scales the matching ASG. See docs/design/agent-platform.md (R-PROV-3).

// ClusterAutoscalerConfigMapName is the operator-owned ConfigMap that
// carries the rendered node-group spec for anp.
func ClusterAutoscalerConfigMapName(anp *v1.AgentNodePool) string {
	return "anp-" + anp.Name + "-clusterautoscaler"
}

// BuildClusterAutoscalerConfigMap renders the externally-managed node-group
// spec for a ClusterAutoscaler AgentNodePool.
func BuildClusterAutoscalerConfigMap(anp *v1.AgentNodePool, ns string, defaults KarpenterDefaults) *corev1.ConfigMap {
	// CAS scale-up simulation discovers ASGs and learns their would-be node
	// labels/taints from these tags; they must mirror the coupling keys so a
	// pending kata pod scales the right ASG.
	tags := strings.Join([]string{
		`k8s.io/cluster-autoscaler/enabled: "true"`,
		fmt.Sprintf("k8s.io/cluster-autoscaler/node-template/label/%s: %q", PoolLabelKey, anp.Name),
		fmt.Sprintf("k8s.io/cluster-autoscaler/node-template/taint/%s: %q", IsolationTaintKey, anp.Spec.Isolation+":NoSchedule"),
	}, "\n")

	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClusterAutoscalerConfigMapName(anp),
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": karpenterManagedBy,
				PoolLabelKey:                   anp.Name,
			},
		},
		Data: map[string]string{
			"provider":         "cluster-autoscaler",
			"isolation":        anp.Spec.Isolation,
			"arch":             orDefault(anp.Spec.Arch, "arm64"),
			"instanceFamilies": strings.Join(anp.Spec.InstanceFamilies, ","),
			"capacityType":     strings.Join(anp.Spec.CapacityType, ","),
			"minNodes":         fmt.Sprintf("%d", anp.Spec.MinNodes),
			// The coupling keys a kata pod carries; the ASG must surface them.
			"poolLabel":      fmt.Sprintf("%s=%s", PoolLabelKey, anp.Name),
			"isolationTaint": fmt.Sprintf("%s=%s:NoSchedule", IsolationTaintKey, anp.Spec.Isolation),
			// Tags the ASG must carry for CAS auto-discovery + node-template.
			"requiredASGTags": tags,
			// Launch-template userData: the same kata layer composed onto the
			// existing node-join as the Karpenter EC2NodeClass.
			"userData": composeUserData(anp, defaults),
		},
	}
}
