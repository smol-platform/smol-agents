package builders

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

func TestAgentSAName(t *testing.T) {
	// One source of truth for the SA name BuildAgentRunPod references.
	if got := AgentSAName("alice"); got != "alice-agent" {
		t.Errorf("AgentSAName(alice) = %q, want alice-agent", got)
	}
}

func TestAgentServiceAccount(t *testing.T) {
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "tenant-a"}}
	sa := AgentServiceAccount(a)

	if sa.Name != "alice-agent" {
		t.Errorf("name = %q, want alice-agent", sa.Name)
	}
	if sa.Namespace != "tenant-a" {
		t.Errorf("namespace = %q", sa.Namespace)
	}
	if sa.Labels["agents.smol-agents.ai/agent"] != "alice" {
		t.Errorf("agent label = %q", sa.Labels["agents.smol-agents.ai/agent"])
	}
	// Sanity: the SA name must match what BuildAgentRunPod looks for.
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "tenant-a"}}
	r.Spec.AgentRef = "alice"
	pod := BuildAgentRunPod(r, a)
	if pod.Spec.ServiceAccountName != sa.Name {
		t.Errorf("BuildAgentRunPod.ServiceAccountName=%q != AgentServiceAccount.Name=%q",
			pod.Spec.ServiceAccountName, sa.Name)
	}
}
