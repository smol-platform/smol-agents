package builders

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestApplyPodTemplatePlacement(t *testing.T) {
	tpl := &corev1.PodTemplateSpec{}
	ApplyPodTemplatePlacement(tpl, NodePlacement{PoolName: "kata-arm64", Isolation: "kata-fc"})

	na := tpl.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	me := na.NodeSelectorTerms[0].MatchExpressions[0]
	if me.Key != PoolLabelKey || len(me.Values) != 1 || me.Values[0] != "kata-arm64" {
		t.Errorf("nodeAffinity = %+v, want %s In [kata-arm64]", me, PoolLabelKey)
	}
	if len(tpl.Spec.Tolerations) != 1 ||
		tpl.Spec.Tolerations[0].Key != IsolationTaintKey ||
		tpl.Spec.Tolerations[0].Value != "kata-fc" {
		t.Errorf("toleration = %+v", tpl.Spec.Tolerations)
	}
	if tpl.ObjectMeta.Annotations[DoNotDisruptAnnotation] != "true" {
		t.Error("do-not-disrupt annotation missing")
	}
}

func TestApplyPodTemplatePlacement_NoopWhenEmpty(t *testing.T) {
	tpl := &corev1.PodTemplateSpec{}
	ApplyPodTemplatePlacement(tpl, NodePlacement{})
	if tpl.Spec.Affinity != nil || len(tpl.Spec.Tolerations) != 0 {
		t.Error("expected no placement for empty pool name")
	}
}

func TestApplyKnativePlacement(t *testing.T) {
	u := BuildKnativeService(sample())
	ApplyKnativePlacement(u, NodePlacement{PoolName: "kata-arm64", Isolation: "kata-fc"})

	tpl := u.Object["spec"].(map[string]any)["template"].(map[string]any)
	tplSpec := tpl["spec"].(map[string]any)

	terms := tplSpec["affinity"].(map[string]any)["nodeAffinity"].(map[string]any)["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)["nodeSelectorTerms"].([]any)
	me := terms[0].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)
	if me["key"] != PoolLabelKey || me["values"].([]any)[0] != "kata-arm64" {
		t.Errorf("knative nodeAffinity = %v", me)
	}
	tol := tplSpec["tolerations"].([]any)[0].(map[string]any)
	if tol["key"] != IsolationTaintKey || tol["value"] != "kata-fc" {
		t.Errorf("knative toleration = %v", tol)
	}
	ann := tpl["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann[DoNotDisruptAnnotation] != "true" {
		t.Error("knative do-not-disrupt annotation missing")
	}
}
