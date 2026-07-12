package v1

import (
	"errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AttachGrant is the durable authorization record for a human attaching to an
// agent's interactive terminal (M4.6, D5). It names the agent, the human
// subject, the role (viewer|driver), and a hard expiry. The cmd/agentterminal
// gateway resolves a live, unexpired grant before minting an audience-bound
// attach token (pkg/attachtoken). It is namespaced, so a tenant's RBAC on
// attachgrants scopes who can grant — and a grant can only reference an agent in
// its own namespace (no cross-tenant driver grant).
type AttachGrant struct {
	Name   string            `json:"name"`
	Spec   AttachGrantSpec   `json:"spec"`
	Status AttachGrantStatus `json:"status,omitempty"`
}

const (
	// AttachRoleViewer is a read-only attach (no keystrokes reach the PTY).
	AttachRoleViewer = "viewer"
	// AttachRoleDriver is a read/write attach (drives the terminal). Driver
	// grants are the privileged ones — mandatory recording, tighter RBAC.
	AttachRoleDriver = "driver"
)

type AttachGrantSpec struct {
	// AgentRef is the Agent in THIS namespace the grant authorizes
	// attach to. Cross-namespace references are rejected (no cross-tenant grant).
	AgentRef string `json:"agentRef"`
	// Subject is the human identity (OIDC subject / email) the grant is for.
	Subject string `json:"subject"`
	// Role is viewer (read-only) or driver (read/write).
	// +kubebuilder:validation:Enum=viewer;driver
	Role string `json:"role"`
	// ExpiresAt is the hard expiry of the grant; the gateway never mints a token
	// outliving it. A nil/zero value is rejected (a grant must expire).
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

type AttachGrantStatus struct {
	// LastAttachTime is the most recent attach the gateway authorized via this
	// grant (observability / audit correlation).
	// +optional
	LastAttachTime     *metav1.Time `json:"lastAttachTime,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
}

// ValidateAttachGrant checks an AttachGrant's self-consistency (admission-time,
// no cluster lookups): a valid role, a subject, an agentRef with no cross-
// namespace reference, and a non-empty expiry.
func ValidateAttachGrant(g AttachGrant) error {
	var errs []error
	switch g.Spec.Role {
	case AttachRoleViewer, AttachRoleDriver:
	default:
		errs = append(errs, errors.New("spec.role must be viewer or driver"))
	}
	if g.Spec.Subject == "" {
		errs = append(errs, errors.New("spec.subject is required"))
	}
	if g.Spec.AgentRef == "" {
		errs = append(errs, errors.New("spec.agentRef is required"))
	} else if containsSlash(g.Spec.AgentRef) {
		// A bare name only — a "<ns>/<name>" form would let a grant point across
		// namespaces (cross-tenant driver grant). Reject it.
		errs = append(errs, errors.New("spec.agentRef must be a bare name in this namespace (no cross-namespace reference)"))
	}
	if g.Spec.ExpiresAt == nil || g.Spec.ExpiresAt.IsZero() {
		errs = append(errs, errors.New("spec.expiresAt is required (a grant must expire)"))
	}
	return errors.Join(errs...)
}

func containsSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}
