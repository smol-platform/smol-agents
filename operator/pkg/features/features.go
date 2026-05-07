// Package features is the single source of truth for the operator's
// feature-flag identifiers. Every reconciler, webhook, and Status
// condition references constants from this package — adding a new
// feature means adding a constant here and registering a reconciler.
//
// Implements R-OP-FF-5.
package features

import "fmt"

// Result is what each FeatureReconciler returns. Lives here (rather than
// in the controllers package) so that features.* reconcilers can import
// without creating a cycle. Implements R-OP-FF-3.
type Result struct {
	Feature Feature
	Enabled bool
	Ready   bool
	Reason  string // short stable token, e.g. "Disabled" | "PrerequisitesUnmet" | "Reconciled"
	Message string // free text
	Mode    string // optional (e.g. for identity)
	Err     error  // non-nil only on hard errors
}

// Feature is the canonical identifier for a platform capability.
type Feature string

const (
	Identity         Feature = "identity"
	TransportPrivate Feature = "transport.private"
	TransportPublic  Feature = "transport.public"
	Secrets          Feature = "secrets"
	Sandbox          Feature = "sandbox"
	EBPF             Feature = "ebpf"
	Knative          Feature = "knative"
	Observability    Feature = "observability"
)

// All returns every supported feature in stable order. The order is
// also the default reconcile order (identity → transport → secrets → …).
func All() []Feature {
	return []Feature{
		Identity, TransportPrivate, TransportPublic,
		Secrets, Sandbox, EBPF, Knative, Observability,
	}
}

// Valid reports whether f is a known feature.
func Valid(f Feature) bool {
	for _, k := range All() {
		if k == f {
			return true
		}
	}
	return false
}

// ConditionType is the metav1.Condition.Type for the feature's Ready
// condition (e.g. `IdentityReady`, `TransportPrivateReady`). Used by
// every Status update.
//
// We deliberately camel-case here because Kubernetes conditions use that
// convention.
func ConditionType(f Feature) string {
	switch f {
	case Identity:
		return "IdentityReady"
	case TransportPrivate:
		return "TransportPrivateReady"
	case TransportPublic:
		return "TransportPublicReady"
	case Secrets:
		return "SecretsReady"
	case Sandbox:
		return "SandboxReady"
	case EBPF:
		return "EBPFReady"
	case Knative:
		return "KnativeReady"
	case Observability:
		return "ObservabilityReady"
	default:
		return fmt.Sprintf("Unknown_%s_Ready", f)
	}
}
