package builders

import (
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildRuntimeClassKataFC returns a RuntimeClass named "kata-fc" whose
// handler is "kata-fc" — matching the kata-deploy chart convention.
// Idempotent at apply time. R-SBX-1.
//
// Overhead accounts for the Firecracker microVM's own memory/CPU so the
// scheduler — and Karpenter, when sizing nodes — reserves room for it on
// top of the pod's requests (R-PROV-1).
func BuildRuntimeClassKataFC() *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "node.k8s.io/v1", Kind: "RuntimeClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "kata-fc"},
		Handler:    "kata-fc",
		Overhead: &nodev1.Overhead{
			PodFixed: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}
}

// BuildRuntimeClassGVisor returns a gVisor RuntimeClass for clusters
// that prefer userspace sandboxing (or where KVM is unavailable, e.g.
// GKE managed nodes).
func BuildRuntimeClassGVisor() *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "node.k8s.io/v1", Kind: "RuntimeClass"},
		ObjectMeta: metav1.ObjectMeta{Name: "gvisor"},
		Handler:    "runsc",
	}
}
