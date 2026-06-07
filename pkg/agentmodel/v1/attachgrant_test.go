package v1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validGrant() AttachGrant {
	exp := metav1.Now()
	return AttachGrant{
		Name: "g1",
		Spec: AttachGrantSpec{AgentRef: "claude", Subject: "alice@example.com", Role: AttachRoleDriver, ExpiresAt: &exp},
	}
}

func TestValidateAttachGrant(t *testing.T) {
	if err := ValidateAttachGrant(validGrant()); err != nil {
		t.Fatalf("valid grant rejected: %v", err)
	}

	bad := validGrant()
	bad.Spec.Role = "admin"
	if err := ValidateAttachGrant(bad); err == nil || !strings.Contains(err.Error(), "role") {
		t.Errorf("invalid role must be rejected, got %v", err)
	}

	// Cross-namespace agentRef ("ns/name") is rejected — no cross-tenant grant.
	cross := validGrant()
	cross.Spec.AgentRef = "tenant-b/claude"
	if err := ValidateAttachGrant(cross); err == nil || !strings.Contains(err.Error(), "cross-namespace") {
		t.Errorf("cross-namespace agentRef must be rejected, got %v", err)
	}

	noSub := validGrant()
	noSub.Spec.Subject = ""
	if err := ValidateAttachGrant(noSub); err == nil {
		t.Error("missing subject must be rejected")
	}

	noExp := validGrant()
	noExp.Spec.ExpiresAt = nil
	if err := ValidateAttachGrant(noExp); err == nil || !strings.Contains(err.Error(), "expiresAt") {
		t.Errorf("missing expiry must be rejected, got %v", err)
	}
}
