package builders

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1 "github.com/stigen/smol-agents/operator/api/v1"
)

// ClusterSPIFFEIDGVR is the cluster-scoped CRD shipped by spiffe.io.
var ClusterSPIFFEIDGVR = schema.GroupVersionKind{
	Group:   "spire.spiffe.io",
	Version: "v1alpha1",
	Kind:    "ClusterSPIFFEID",
}

// BuildClusterSPIFFEID renders a ClusterSPIFFEID for the agent. Returned
// as Unstructured so we don't take a build-time dep on the SPIRE CRD
// types. All map values are stringly-typed at the leaves to keep
// runtime.Object DeepCopy happy (Unstructured's DeepCopy refuses to
// recurse into typed maps like map[string]string nested inside a
// map[string]any).
func BuildClusterSPIFFEID(cr *v1.SmolAgent) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ClusterSPIFFEIDGVR)
	u.SetName(cr.Namespace + "-" + cr.Name)
	u.SetLabels(Labels(cr))

	matchLabels := map[string]any{}
	for k, v := range Selector(cr) {
		matchLabels[k] = v
	}
	u.Object["spec"] = map[string]any{
		"spiffeIDTemplate": fmt.Sprintf("spiffe://%s/ns/{{ .PodMeta.Namespace }}/sa/{{ .PodSpec.ServiceAccountName }}",
			cr.Spec.TrustDomain),
		"podSelector": map[string]any{
			"matchLabels": matchLabels,
		},
		"workloadSelectorTemplates": []any{
			"k8s:ns:" + cr.Namespace,
			"k8s:sa:" + cr.Name + "-agent",
		},
	}
	return u
}
