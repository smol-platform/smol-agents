package builders

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

func teamEnv(pod *corev1.Pod) map[string]string {
	m := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		m[e.Name] = e.Value
	}
	return m
}

// rv3.1: a loop run carrying the team labels gets the team NATS context so its
// kind=task/kind=teammate/kind=teambus invokers can bind the shared task list +
// mailbox. Member identity comes from the team-member label.
func TestBuildAgentRunPod_TeamEnv(t *testing.T) {
	t.Setenv("SMOL_AGENTS_TEAM_NATS_URL", "nats://team-nats:4222")
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "tenant-a"}}
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Name: "m1", Namespace: "tenant-a",
		Labels: map[string]string{TeamLabel: "squad", TeamMemberLabel: "researcher"},
	}}
	r.Spec.AgentRef = "worker"

	env := teamEnv(BuildAgentRunPod(r, a))
	if env["TEAM_NATS_URL"] != "nats://team-nats:4222" {
		t.Errorf("TEAM_NATS_URL = %q, want nats://team-nats:4222", env["TEAM_NATS_URL"])
	}
	if env["TEAM_NAMESPACE"] != "tenant-a" {
		t.Errorf("TEAM_NAMESPACE = %q, want tenant-a", env["TEAM_NAMESPACE"])
	}
	if env["TEAM_NAME"] != "squad" {
		t.Errorf("TEAM_NAME = %q, want squad", env["TEAM_NAME"])
	}
	if env["TEAM_MEMBER"] != "researcher" {
		t.Errorf("TEAM_MEMBER = %q, want researcher", env["TEAM_MEMBER"])
	}
}

// Fail-closed: a team run with no operator --team-nats-url gets NO team env, so
// the team invokers stay absent and the executor fail-closes the call.
func TestBuildAgentRunPod_TeamEnv_NoURLFailsClosed(t *testing.T) {
	t.Setenv("SMOL_AGENTS_TEAM_NATS_URL", "")
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "t"}}
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Name: "m1", Namespace: "t",
		Labels: map[string]string{TeamLabel: "squad", TeamMemberLabel: "researcher"},
	}}
	r.Spec.AgentRef = "worker"
	if _, ok := teamEnv(BuildAgentRunPod(r, a))["TEAM_NATS_URL"]; ok {
		t.Error("TEAM_NATS_URL present without --team-nats-url, want fail-closed (absent)")
	}
}

// A non-team run never gets team env, even when the operator has a team NATS URL.
func TestBuildAgentRunPod_TeamEnv_NoLabelAbsent(t *testing.T) {
	t.Setenv("SMOL_AGENTS_TEAM_NATS_URL", "nats://team-nats:4222")
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "t"}}
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "t"}}
	r.Spec.AgentRef = "solo"
	if _, ok := teamEnv(BuildAgentRunPod(r, a))["TEAM_NAME"]; ok {
		t.Error("TEAM_NAME present on a non-team run, want absent")
	}
}

// The lead (team label, no member label) still gets team context; TEAM_MEMBER is
// absent (kind=task falls back to RUN_NAME).
func TestBuildAgentRunPod_TeamEnv_LeadNoMember(t *testing.T) {
	t.Setenv("SMOL_AGENTS_TEAM_NATS_URL", "nats://team-nats:4222")
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "lead", Namespace: "t"}}
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{
		Name: "lead-run", Namespace: "t",
		Labels: map[string]string{TeamLabel: "squad"},
	}}
	r.Spec.AgentRef = "lead"
	env := teamEnv(BuildAgentRunPod(r, a))
	if env["TEAM_NAME"] != "squad" {
		t.Errorf("TEAM_NAME = %q, want squad", env["TEAM_NAME"])
	}
	if _, ok := env["TEAM_MEMBER"]; ok {
		t.Error("TEAM_MEMBER present for a lead with no member label, want absent")
	}
}
