package webhooks

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// A redaction pattern that does not compile must be rejected at admission, not
// silently skipped on the fold path (which would quietly weaken redaction).
func TestAgentPolicySelfWebhook_RejectsBadRegex(t *testing.T) {
	ap := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec:       pure.AgentPolicySpec{Redaction: &pure.RedactionPolicy{Patterns: []string{"("}}},
	}
	if _, err := (agentPolicySelf{}).ValidateCreate(context.Background(), ap); err == nil {
		t.Fatal("expected an uncompilable redaction pattern to be rejected")
	}
}

func TestAgentPolicySelfWebhook_AcceptsValid(t *testing.T) {
	ap := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p"},
		Spec: pure.AgentPolicySpec{
			AllowedProviders: []string{"openai"},
			Redaction:        &pure.RedactionPolicy{Patterns: []string{`sk-[a-z0-9]+`}},
		},
	}
	v := agentPolicySelf{}
	if _, err := v.ValidateCreate(context.Background(), ap); err != nil {
		t.Fatalf("valid policy rejected on create: %v", err)
	}
	if _, err := v.ValidateUpdate(context.Background(), nil, ap); err != nil {
		t.Fatalf("valid policy rejected on update: %v", err)
	}
}
