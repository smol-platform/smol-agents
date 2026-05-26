package builders

import (
	"encoding/json"
	"testing"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestBuildAgentRunPod_RunSpecWiring(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"
	agent := &amv1.Agent{}
	agent.Name = "a1"
	agent.Namespace = "tenant-a"
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessHermes, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}

	pod := BuildAgentRunPod(run, agent)
	c := pod.Spec.Containers[0]

	// Executes `agent run` against the mounted spec, regardless of mode.
	if len(c.Command) != 3 || c.Command[0] != "/agent" || c.Command[1] != "run" {
		t.Errorf("run command = %v", c.Command)
	}
	if len(c.Args) != 0 {
		t.Errorf("Args should be cleared, got %v", c.Args)
	}
	if !hasVolume(pod, runSpecVolumeName) {
		t.Error("runspec volume missing")
	}
	if _, ok := hasMount(c, runSpecVolumeName); !ok {
		t.Error("runspec mount missing on run container")
	}
}

func TestBuildRunSpecConfigMap(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"
	run.Spec = pure.AgentRunSpec{AgentRef: "a1", Input: json.RawMessage(`{"prompt":"hi"}`), Seed: 7}
	agent := &amv1.Agent{}
	agent.Name = "a1"
	agent.Namespace = "tenant-a"
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Instructions = "be terse"
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessHermes, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}

	cm, err := BuildRunSpecConfigMap(run, agent)
	if err != nil {
		t.Fatalf("BuildRunSpecConfigMap: %v", err)
	}
	if cm.Name != "r1-runspec" || cm.Namespace != "tenant-a" {
		t.Errorf("name/ns = %s/%s", cm.Namespace, cm.Name)
	}

	// agent.json round-trips to a pure Agent carrying the harness + instructions.
	var a pure.Agent
	if err := json.Unmarshal([]byte(cm.Data[runSpecAgentFile]), &a); err != nil {
		t.Fatalf("agent.json: %v", err)
	}
	if a.Spec.Harness == nil || a.Spec.Harness.Kind != pure.HarnessHermes {
		t.Errorf("agent.json lost harness: %+v", a.Spec)
	}
	if a.Spec.Instructions != "be terse" {
		t.Errorf("agent.json lost instructions")
	}

	// run.json round-trips to the AgentRunSpec.
	var rs pure.AgentRunSpec
	if err := json.Unmarshal([]byte(cm.Data[runSpecRunFile]), &rs); err != nil {
		t.Fatalf("run.json: %v", err)
	}
	if rs.Seed != 7 || string(rs.Input) != `{"prompt":"hi"}` {
		t.Errorf("run.json wrong: %+v", rs)
	}
}
