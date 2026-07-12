package builders

import (
	corev1 "k8s.io/api/core/v1"
)

// NodePlacement binds an agent's pod to its AgentNodePool: the pool's node
// label (for nodeAffinity) and the isolation taint it must tolerate. The
// controller resolves it (auto-match by isolation) and applies it to every
// workload kind so kata pods land only on kata-capable nodes. R-PROV-2.
type NodePlacement struct {
	PoolName  string
	Isolation string
}

// DoNotDisruptAnnotation tells Karpenter never to voluntarily disrupt the
// node running this pod — a live Firecracker microVM must not be
// consolidated out from under running work. R-PROV-5.
const DoNotDisruptAnnotation = "karpenter.sh/do-not-disrupt"

func placementNodeAffinity(p NodePlacement) *corev1.NodeAffinity {
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      PoolLabelKey,
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{p.PoolName},
				}},
			}},
		},
	}
}

func placementToleration(p NodePlacement) corev1.Toleration {
	return corev1.Toleration{
		Key:      IsolationTaintKey,
		Operator: corev1.TolerationOpEqual,
		Value:    p.Isolation,
		Effect:   corev1.TaintEffectNoSchedule,
	}
}
