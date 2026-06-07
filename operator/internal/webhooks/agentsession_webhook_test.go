package webhooks

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func sessionWebhookWith(t *testing.T, objs ...client.Object) *agentSessionWebhook {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return &agentSessionWebhook{client: fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()}
}

func TestAgentSessionWebhook_AgentRefMustExist(t *testing.T) {
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "t"}}
	w := sessionWebhookWith(t, agent)

	good := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "t"}}
	good.Spec.AgentRef = "alice"
	if _, err := w.ValidateCreate(context.Background(), good); err != nil {
		t.Errorf("existing agentRef must pass: %v", err)
	}

	dangling := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "t"}}
	dangling.Spec.AgentRef = "ghost"
	if _, err := w.ValidateCreate(context.Background(), dangling); err == nil {
		t.Errorf("dangling agentRef must be rejected")
	}

	empty := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "t"}}
	if _, err := w.ValidateCreate(context.Background(), empty); err == nil {
		t.Errorf("empty agentRef must be rejected")
	}
}

func TestAgentSessionWebhook_RejectsBadResourceQuantity(t *testing.T) {
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "t"}}
	w := sessionWebhookWith(t, agent)

	bad := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "t"}}
	bad.Spec.AgentRef = "alice"
	bad.Spec.Resources = &pure.ResourceRequirements{Limits: map[string]string{"memory": "500x"}}
	if _, err := w.ValidateCreate(context.Background(), bad); err == nil {
		t.Errorf("unparseable resource quantity must be rejected at admission")
	}

	good := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "t"}}
	good.Spec.AgentRef = "alice"
	good.Spec.Resources = &pure.ResourceRequirements{
		Limits:   map[string]string{"memory": "512Mi", "cpu": "500m"},
		Requests: map[string]string{"memory": "256Mi"},
	}
	if _, err := w.ValidateCreate(context.Background(), good); err != nil {
		t.Errorf("valid resource quantities must pass: %v", err)
	}
}

// M2.22: a per-turn delivery timeout exceeding the turn-stream retention is
// rejected (only when both are explicitly set; defaults stay valid).
func TestAgentSessionWebhook_RejectsTimeoutOverRetention(t *testing.T) {
	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "t"}}
	w := sessionWebhookWith(t, agent)

	bad := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "t"}}
	bad.Spec.AgentRef = "alice"
	bad.Spec.TurnDeliveryTimeoutSeconds = 600
	bad.Spec.TurnRetentionSeconds = 300
	if _, err := w.ValidateCreate(context.Background(), bad); err == nil {
		t.Error("delivery timeout > retention must be rejected")
	}

	ok := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "t"}}
	ok.Spec.AgentRef = "alice"
	ok.Spec.TurnDeliveryTimeoutSeconds = 120
	ok.Spec.TurnRetentionSeconds = 300
	if _, err := w.ValidateCreate(context.Background(), ok); err != nil {
		t.Errorf("delivery <= retention must pass: %v", err)
	}
	// Defaults (both 0) never trip the check.
	def := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "t"}}
	def.Spec.AgentRef = "alice"
	if _, err := w.ValidateCreate(context.Background(), def); err != nil {
		t.Errorf("unset timeouts must pass: %v", err)
	}
}
