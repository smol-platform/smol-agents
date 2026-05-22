package builders

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

// BuildServiceAccount renders the ServiceAccount the agent Pod runs as.
// The ClusterSPIFFEID's workload selector binds to this SA name.
func BuildServiceAccount(cr *v1.SmolAgent) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-agent",
			Namespace: cr.Namespace,
			Labels:    Labels(cr),
		},
	}
}
