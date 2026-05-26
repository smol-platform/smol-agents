package k8s

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func deployment(name string, images ...string) *unstructured.Unstructured {
	cs := make([]interface{}, 0, len(images))
	for i, img := range images {
		cs = append(cs, map[string]interface{}{"name": "c" + string(rune('0'+i)), "image": img})
	}
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]interface{}{"name": name, "namespace": "smol-agents-system"},
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"spec": map[string]interface{}{"containers": cs},
			},
		},
	}}
	return u
}

func imagesOf(t *testing.T, u *unstructured.Unstructured) []string {
	t.Helper()
	cs, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
	var out []string
	for _, c := range cs {
		out = append(out, c.(map[string]interface{})["image"].(string))
	}
	return out
}

func TestOverrideOperatorImage(t *testing.T) {
	op := deployment("smol-agents-operator", "smol-agents/operator:0.1.0", "gcr.io/kubebuilder/kube-rbac-proxy:v0.16")
	crd := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]interface{}{"name": "smolagents.agents.smol-agents.ai"},
	}}
	objs := []*unstructured.Unstructured{op, crd}

	n, err := overrideOperatorImage(objs, "ghcr.io/smol-platform/smol-agents/operator:0.1.0")
	if err != nil {
		t.Fatalf("overrideOperatorImage: %v", err)
	}
	if n != 1 {
		t.Errorf("rewrote %d containers, want 1 (only the operator, not the sidecar)", n)
	}
	got := imagesOf(t, op)
	if got[0] != "ghcr.io/smol-platform/smol-agents/operator:0.1.0" {
		t.Errorf("operator image = %q, not overridden", got[0])
	}
	if got[1] != "gcr.io/kubebuilder/kube-rbac-proxy:v0.16" {
		t.Errorf("sidecar image = %q, should be untouched", got[1])
	}
}

func TestOverrideOperatorImage_EmptyIsNoop(t *testing.T) {
	op := deployment("smol-agents-operator", "smol-agents/operator:0.1.0")
	n, err := overrideOperatorImage([]*unstructured.Unstructured{op}, "")
	if err != nil || n != 0 {
		t.Fatalf("empty override: n=%d err=%v (want 0, nil)", n, err)
	}
	if imagesOf(t, op)[0] != "smol-agents/operator:0.1.0" {
		t.Errorf("image changed despite empty override")
	}
}
