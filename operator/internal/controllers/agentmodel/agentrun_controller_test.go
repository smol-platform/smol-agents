package agentmodel

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// runPodWithTerminationMessage builds a Pod whose `harness` container has
// already terminated with the given RunResult serialised into its termination
// message — the same shape `agent run` writes for the controller to fold.
func runPodWithTerminationMessage(rr agentruntime.RunResult) *corev1.Pod {
	msg, _ := json.Marshal(rr)
	return &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "harness",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 0,
						Reason:   "Completed",
						Message:  string(msg),
					},
				},
			}},
		},
	}
}

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

// markTerminal must own TerminationReason — without this, a stale
// "Pod is Pending" hint left by markPending was surviving into Completed.
func TestAgentRunReconciler_MarkTerminal_ClearsStaleReason(t *testing.T) {
	r := &AgentRunReconciler{}
	run := sampleRun()
	r.markPending(run, "PodPending", "Pod is Pending") // leaves a stale reason
	if run.Status.TerminationReason != "Pod is Pending" {
		t.Fatalf("setup: terminationReason=%q", run.Status.TerminationReason)
	}
	r.markTerminal(run, pure.PhaseCompleted, "")
	if run.Status.TerminationReason != "" {
		t.Errorf("markTerminal(\"\") should clear stale reason, got %q", run.Status.TerminationReason)
	}
}

// The runtime's own reason ("budget:tokens" for an Expired run that still
// exits 0) is the most specific signal and must win over whatever pod-level
// reason markTerminal left behind.
func TestAgentRunReconciler_FoldRunResult_RuntimeReasonWins(t *testing.T) {
	r := &AgentRunReconciler{}
	run := sampleRun()
	r.markTerminal(run, pure.PhaseCompleted, "") // pod said Succeeded, no reason
	pod := runPodWithTerminationMessage(agentruntime.RunResult{
		Phase:             pure.PhaseExpired,
		TerminationReason: "budget:tokens",
		Usage:             pure.Usage{Steps: 1, Tokens: 16102},
	})
	r.foldRunResult(run, pod)
	if run.Status.State != pure.PhaseExpired {
		t.Errorf("state should be refined to Expired, got %q", run.Status.State)
	}
	if run.Status.TerminationReason != "budget:tokens" {
		t.Errorf("terminationReason = %q, want budget:tokens", run.Status.TerminationReason)
	}
	if run.Status.Usage.Tokens != 16102 {
		t.Errorf("usage not folded: tokens=%d", run.Status.Usage.Tokens)
	}
}

// A runtime error wins over both any prior reason and over the runtime's own
// TerminationReason (an Error is more diagnostic than the bare reason).
func TestAgentRunReconciler_FoldRunResult_ErrorWins(t *testing.T) {
	r := &AgentRunReconciler{}
	run := sampleRun()
	r.markTerminal(run, pure.PhaseFailed, "pod:Error")
	pod := runPodWithTerminationMessage(agentruntime.RunResult{
		Phase:             pure.PhaseFailed,
		TerminationReason: "harness:timeout",
		Error:             "harness: http 502: upstream refused",
	})
	r.foldRunResult(run, pod)
	if run.Status.TerminationReason != "harness: http 502: upstream refused" {
		t.Errorf("error should win, got %q", run.Status.TerminationReason)
	}
}

// A clean RunResult (no Error, no TerminationReason) must not clobber what
// markTerminal already set — for a normal Completed run that's the empty
// string, and the status should land empty.
func TestAgentRunReconciler_FoldRunResult_CleanSuccessLeavesEmpty(t *testing.T) {
	r := &AgentRunReconciler{}
	run := sampleRun()
	r.markPending(run, "PodPending", "Pod is Pending")
	r.markTerminal(run, pure.PhaseCompleted, "")
	pod := runPodWithTerminationMessage(agentruntime.RunResult{
		Phase:  pure.PhaseCompleted,
		Output: json.RawMessage(`{"answer":42}`),
		Usage:  pure.Usage{Steps: 1, Tokens: 100},
	})
	r.foldRunResult(run, pod)
	if run.Status.TerminationReason != "" {
		t.Errorf("clean success should leave empty terminationReason, got %q", run.Status.TerminationReason)
	}
	if string(run.Status.Output) != `{"answer":42}` {
		t.Errorf("output not folded: %s", run.Status.Output)
	}
}
