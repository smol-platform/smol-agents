package builders

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

// A loop run pod carries the A2A identity its in-pod invoker needs: its own
// namespace + run name + the AgentRun's OWN uid. The uid MUST be a literal — the
// pod's downward-API metadata.uid is the POD's uid, not the run's, and a wrong
// uid makes k8s GC delete child AgentRuns immediately (owner looks non-existent).
func TestBuildAgentRunPod_A2AEnv(t *testing.T) {
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "tenant-a"}}
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "tenant-a", UID: types.UID("uid-abc")}}
	r.Spec.AgentRef = "parent"
	pod := BuildAgentRunPod(r, a)

	env := map[string]string{}
	uidLiteral := false
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
		if e.Name == "AGENT_RUN_UID" {
			if e.ValueFrom != nil {
				t.Errorf("AGENT_RUN_UID must be a literal (the run uid), not a fieldRef")
			}
			uidLiteral = true
		}
	}
	if !uidLiteral || env["AGENT_RUN_UID"] != "uid-abc" {
		t.Errorf("AGENT_RUN_UID = %q (present=%v), want literal run uid uid-abc", env["AGENT_RUN_UID"], uidLiteral)
	}
	if env["RUN_NAME"] != "run-1" {
		t.Errorf("RUN_NAME = %q, want run-1", env["RUN_NAME"])
	}
	// M3.5: the recursion ceiling defaults to 4 and honors SMOL_AGENTS_A2A_MAX_DEPTH.
	if env["A2A_MAX_DEPTH"] != "4" {
		t.Errorf("A2A_MAX_DEPTH = %q, want default 4", env["A2A_MAX_DEPTH"])
	}
}

func TestBuildAgentRunPod_A2AMaxDepthEnvOverride(t *testing.T) {
	t.Setenv("SMOL_AGENTS_A2A_MAX_DEPTH", "7")
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "t"}}
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "t"}}
	r.Spec.AgentRef = "p"
	pod := BuildAgentRunPod(r, a)
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "A2A_MAX_DEPTH" && e.Value != "7" {
			t.Errorf("A2A_MAX_DEPTH = %q, want 7 (env override)", e.Value)
		}
	}
}
