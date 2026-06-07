package webhooks

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestAgentTeamWebhook(t *testing.T) {
	w := &agentTeamWebhook{}
	good := &amv1.AgentTeam{
		ObjectMeta: metav1.ObjectMeta{Name: "team1", Namespace: "t"},
		Spec: pure.AgentTeamSpec{
			Lead:    "coordinator",
			Members: []pure.TeamMemberSpec{{Name: "m1", AgentRef: "a1"}},
		},
	}
	if _, err := w.ValidateCreate(context.Background(), good); err != nil {
		t.Fatalf("valid team rejected at admission: %v", err)
	}

	// Cross-namespace member ref → rejected at admission (self-contained check).
	bad := good.DeepCopy()
	bad.Spec.Members[0].AgentRef = "other/agent"
	if _, err := w.ValidateCreate(context.Background(), bad); err == nil {
		t.Fatalf("cross-namespace member agentRef must be rejected")
	}

	// No members → rejected.
	empty := good.DeepCopy()
	empty.Spec.Members = nil
	if _, err := w.ValidateUpdate(context.Background(), good, empty); err == nil {
		t.Fatalf("team with no members must be rejected")
	}
}
