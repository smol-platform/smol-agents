package sandbox

import (
	"errors"
	"fmt"
)

// Kind classifies a sandbox runtime by its isolation technology, so
// callers can reason about safety properties without string-matching
// RuntimeClass names. Implements R-SBX-1.
type Kind string

const (
	// KindKataFC is the production default: Kata Containers with the
	// Firecracker microVM hypervisor (`runtimeClassName: kata-fc`).
	KindKataFC Kind = "kata-fc"

	// KindKataQEMU is the QEMU-backed Kata variant. Heavier than FC but
	// supports more device types (used as fallback on older kernels).
	KindKataQEMU Kind = "kata-qemu"

	// KindKataCLH is Kata + Cloud Hypervisor.
	KindKataCLH Kind = "kata-clh"

	// KindGVisor is Google's userspace sandbox. Retained as the
	// fallback for managed K8s services that do not expose KVM (notably
	// GKE, where gVisor IS the supported sandbox).
	KindGVisor Kind = "gvisor"

	// KindRunc is the standard OCI runtime — NOT a sandbox. Selecting
	// it requires AllowHostEscape and bypasses R-SBX-1.
	KindRunc Kind = "runc"
)

// Valid returns true if k is a known Kind.
func (k Kind) Valid() bool {
	switch k {
	case KindKataFC, KindKataQEMU, KindKataCLH, KindGVisor, KindRunc:
		return true
	}
	return false
}

// IsMicroVM reports whether the kind uses hardware virtualization
// (KVM-backed microVM). Affects host prerequisites: micro-VM kinds need
// `/dev/kvm` and either bare metal or nested virtualization.
func (k Kind) IsMicroVM() bool {
	switch k {
	case KindKataFC, KindKataQEMU, KindKataCLH:
		return true
	}
	return false
}

// IsHardened returns true for any non-runc kind. Runc explicitly leaks
// the host kernel into the workload.
func (k Kind) IsHardened() bool { return k != KindRunc }

// ParseKind maps a RuntimeClass name to a Kind. Unknown names default
// to KindRunc so the validator's R-SBX-1 guard fires.
func ParseKind(runtimeClass string) Kind {
	k := Kind(runtimeClass)
	if k.Valid() {
		return k
	}
	return KindRunc
}

// Spec captures the sandbox parameters surfaced into Pod manifests.
type Spec struct {
	// RuntimeClass is the Kubernetes RuntimeClass name. The string IS
	// the Kind — see Kind constants.
	RuntimeClass string

	// AllowHostEscape, when true, permits a deployment to run under
	// runc (no sandbox). Charts must surface this as a deliberate
	// override and refuse silently. R-SBX-1 acceptance #2.
	AllowHostEscape bool
}

// DefaultSpec returns the production default: Kata + Firecracker, no escape.
func DefaultSpec() Spec {
	return Spec{RuntimeClass: string(KindKataFC), AllowHostEscape: false}
}

// Validate checks that Spec is internally consistent.
func (s Spec) Validate() error {
	if s.RuntimeClass == "" {
		return errors.New("sandbox: RuntimeClass is required")
	}
	k := ParseKind(s.RuntimeClass)
	if !k.Valid() {
		return fmt.Errorf("sandbox: RuntimeClass=%q is not a recognised Kind", s.RuntimeClass)
	}
	if k == KindRunc && !s.AllowHostEscape {
		return fmt.Errorf("sandbox: RuntimeClass=runc requires AllowHostEscape=true (R-SBX-1)")
	}
	return nil
}

// Kind returns the parsed Kind for this spec.
func (s Spec) Kind() Kind { return ParseKind(s.RuntimeClass) }

// IsHardened reports whether the spec uses a sandbox runtime
// (anything that is not runc).
func (s Spec) IsHardened() bool { return s.Kind().IsHardened() }
