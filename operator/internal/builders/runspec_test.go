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

// M2.10: tools.json is written only when tools are present (nil-tools path is
// byte-identical to the original), and a too-large catalog is rejected.
func TestBuildRunSpecConfigMapWithTools(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"
	run.Spec = pure.AgentRunSpec{AgentRef: "a1", Input: json.RawMessage(`{}`)}
	agent := &amv1.Agent{}
	agent.Name = "a1"
	agent.Namespace = "tenant-a"
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Instructions = "x"
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessHermes, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}

	// nil tools → no tools.json key.
	cmNil, err := BuildRunSpecConfigMapWithTools(run, agent, nil, nil)
	if err != nil {
		t.Fatalf("nil tools: %v", err)
	}
	if _, ok := cmNil.Data[runSpecToolsFile]; ok {
		t.Errorf("nil tools must not write tools.json")
	}

	// real tools → tools.json round-trips.
	tools := []pure.Tool{{Name: "search", Spec: pure.ToolSpec{Kind: pure.ToolHTTP}}}
	cm, err := BuildRunSpecConfigMapWithTools(run, agent, nil, tools)
	if err != nil {
		t.Fatalf("with tools: %v", err)
	}
	var got []pure.Tool
	if err := json.Unmarshal([]byte(cm.Data[runSpecToolsFile]), &got); err != nil {
		t.Fatalf("tools.json: %v", err)
	}
	if len(got) != 1 || got[0].Name != "search" {
		t.Errorf("tools.json round-trip wrong: %+v", got)
	}

	// oversized catalog → ToolSpecTooLarge error.
	big := make([]pure.Tool, 0, 4000)
	for i := 0; i < 4000; i++ {
		big = append(big, pure.Tool{Name: "t", Spec: pure.ToolSpec{Kind: pure.ToolHTTP, HTTP: &pure.HTTPSpec{URL: "https://example.com/" + string(make([]byte, 256))}}})
	}
	if _, err := BuildRunSpecConfigMapWithTools(run, agent, nil, big); err == nil {
		t.Errorf("an oversized tool catalog must be rejected")
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

	cm, err := BuildRunSpecConfigMap(run, agent, nil)
	if err != nil {
		t.Fatalf("BuildRunSpecConfigMap: %v", err)
	}
	if cm.Name != "r1-runspec" || cm.Namespace != "tenant-a" {
		t.Errorf("name/ns = %s/%s", cm.Namespace, cm.Name)
	}
	if _, ok := cm.Data[runSpecProviderFile]; ok {
		t.Error("no provider → provider.json should be absent")
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

func TestBuildRunSpecConfigMap_Provider(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"
	agent := &amv1.Agent{}
	agent.Name = "a1"
	agent.Namespace = "tenant-a"
	agent.Spec.Model = pure.ModelRef{ProviderRef: "openai", Name: "gpt-4"}

	cm, err := BuildRunSpecConfigMap(run, agent, &RunProvider{
		Kind: "openai", Endpoint: "https://api.openai.com", SecretName: "openai-key",
	})
	if err != nil {
		t.Fatalf("BuildRunSpecConfigMap: %v", err)
	}
	var p RunProvider
	if err := json.Unmarshal([]byte(cm.Data[runSpecProviderFile]), &p); err != nil {
		t.Fatalf("provider.json: %v", err)
	}
	if p.Kind != "openai" || p.Endpoint != "https://api.openai.com" || p.SecretName != "openai-key" {
		t.Errorf("provider.json = %+v", p)
	}
}
