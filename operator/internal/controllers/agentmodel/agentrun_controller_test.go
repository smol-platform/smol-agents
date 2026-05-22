package agentmodel

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func sampleAgent() *amv1.Agent {
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "tenant-a"}}
	a.Spec.Mode = pure.ModeLoop
	a.Spec.Model = pure.ModelRef{ProviderRef: "openai", Name: "gpt-4"}
	a.Spec.Instructions = "be helpful"
	a.Spec.Budget = pure.Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 3}
	return a
}

func sampleRun() *amv1.AgentRun {
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-001", Namespace: "tenant-a"}}
	r.Spec.AgentRef = "alice"
	r.Spec.Input = []byte(`{"q":"hi"}`)
	return r
}

func TestBuildAgentRunPod_LoopMode(t *testing.T) {
	pod := builders.BuildAgentRunPod(sampleRun(), sampleAgent())
	if pod.Name != "run-001" || pod.Namespace != "tenant-a" {
		t.Errorf("name/ns wrong: %s/%s", pod.Namespace, pod.Name)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", pod.Spec.RestartPolicy)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d", len(pod.Spec.Containers))
	}
	if pod.Spec.Containers[0].Name != "agent" {
		t.Errorf("loop container name = %s", pod.Spec.Containers[0].Name)
	}
}

func TestBuildAgentRunPod_HarnessMode_Image(t *testing.T) {
	a := sampleAgent()
	a.Spec.Mode = pure.ModeHarness
	a.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessClaudeCode, Image: "myreg/claude:1"}
	pod := builders.BuildAgentRunPod(sampleRun(), a)
	if pod.Spec.Containers[0].Name != "harness" {
		t.Errorf("harness container name = %s", pod.Spec.Containers[0].Name)
	}
	if pod.Spec.Containers[0].Image != "myreg/claude:1" {
		t.Errorf("image override ignored: %s", pod.Spec.Containers[0].Image)
	}
}

func TestBuildAgentRunPod_AgentFSVolume(t *testing.T) {
	a := sampleAgent()
	a.Spec.Mode = pure.ModeHarness
	a.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessClaudeCode}
	a.Spec.Storage = &pure.StorageSpec{
		Kind:    pure.StorageAgentFS,
		AgentFS: &pure.AgentFSSpec{SizeGiB: 5, MountPath: "/var/agentfs"},
	}
	pod := builders.BuildAgentRunPod(sampleRun(), a)
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "agentfs" {
			found = true
		}
	}
	if !found {
		t.Error("agentfs volume not declared")
	}
	mount := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "agentfs" && m.MountPath == "/var/agentfs" {
			mount = true
		}
	}
	if !mount {
		t.Error("agentfs mount missing on harness container")
	}
}

func TestAgentRunReconciler_MarkRunning(t *testing.T) {
	r := &AgentRunReconciler{}
	run := sampleRun()
	r.markRunning(run)
	if run.Status.State != pure.PhaseRunning {
		t.Errorf("state=%s", run.Status.State)
	}
	if run.Status.StartedAt == nil {
		t.Error("StartedAt not set")
	}
	// Idempotent: a second markRunning should not overwrite StartedAt.
	first := *run.Status.StartedAt
	time.Sleep(2 * time.Millisecond)
	r.markRunning(run)
	if !run.Status.StartedAt.Equal(&first) {
		t.Errorf("StartedAt drifted on idempotent mark")
	}
}

func TestAgentRunReconciler_MarkTerminal(t *testing.T) {
	r := &AgentRunReconciler{}
	run := sampleRun()
	r.markTerminal(run, pure.PhaseCompleted, "")
	if run.Status.State != pure.PhaseCompleted {
		t.Errorf("state=%s", run.Status.State)
	}
	if run.Status.EndedAt == nil {
		t.Error("EndedAt not set")
	}
}
