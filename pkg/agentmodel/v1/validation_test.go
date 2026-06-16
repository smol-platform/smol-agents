package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

func goodSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

func TestValidateAgent_RequiresFields(t *testing.T) {
	if err := ValidateAgent(Agent{}); err == nil {
		t.Fatal("empty agent accepted")
	}
}

func TestValidateAgent_HappyPath(t *testing.T) {
	a := Agent{Spec: AgentSpec{
		Model:        ModelRef{ProviderRef: "openai", Name: "gpt-4"},
		Instructions: "be helpful",
		Budget:       Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 30, MaxToolCalls: 3},
	}}
	if err := ValidateAgent(a); err != nil {
		t.Errorf("good agent rejected: %v", err)
	}
}

func TestValidateAgent_BadBudget(t *testing.T) {
	a := Agent{Spec: AgentSpec{
		Model:        ModelRef{ProviderRef: "p", Name: "m"},
		Instructions: "x",
	}}
	err := ValidateAgent(a)
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Errorf("expected budget error, got %v", err)
	}
}

// M5.2: pre-run approval input validation.
func TestValidateAgent_ApprovalTimeout(t *testing.T) {
	base := AgentSpec{
		Model:        ModelRef{ProviderRef: "p", Name: "m"},
		Instructions: "x",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 100, MaxWallClockSeconds: 10, MaxToolCalls: 0},
	}
	bad := base
	bad.Approval = &ApprovalPolicy{RequireApprovalBeforeRun: true, ApprovalTimeoutSeconds: -1}
	if err := ValidateAgent(Agent{Spec: bad}); err == nil {
		t.Errorf("negative approvalTimeoutSeconds must be rejected")
	}
	ok := base
	ok.Approval = &ApprovalPolicy{RequireApprovalBeforeRun: true, ApprovalTimeoutSeconds: 0}
	if err := ValidateAgent(Agent{Spec: ok}); err != nil {
		t.Errorf("zero timeout (= operator default) must be accepted: %v", err)
	}
}

// M4.3: an interactive session needs a resident pod (required=true).
func TestValidateAgent_Session(t *testing.T) {
	base := AgentSpec{
		Model:        ModelRef{ProviderRef: "p", Name: "m"},
		Instructions: "x",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 100, MaxWallClockSeconds: 10, MaxToolCalls: 0},
	}
	bad := base
	bad.Session = &SessionSpec{Interactive: true} // interactive without required
	if err := ValidateAgent(Agent{Spec: bad}); err == nil {
		t.Errorf("interactive session without required must be rejected")
	}
	ok := base
	ok.Session = &SessionSpec{Required: true, Interactive: true}
	if err := ValidateAgent(Agent{Spec: ok}); err != nil {
		t.Errorf("required+interactive must pass: %v", err)
	}
}

// c5r.2: a durable session is loop-only — the session worker has no harness
// runner, so a harness-mode Agent with session.required=true is a silent
// dead-end and must be rejected at validation.
func TestValidateAgent_SessionRequiresLoop(t *testing.T) {
	harnessBase := AgentSpec{
		Mode:         ModeHarness,
		Harness:      &HarnessSpec{Kind: HarnessClaudeCode},
		Instructions: "x",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 100, MaxWallClockSeconds: 10, MaxToolCalls: 0},
	}
	bad := harnessBase
	bad.Session = &SessionSpec{Required: true}
	err := ValidateAgent(Agent{Spec: bad})
	if err == nil || !strings.Contains(err.Error(), "mode=loop") {
		t.Errorf("harness Agent with session.required must be rejected with a mode=loop hint, got %v", err)
	}

	// A loop-mode Agent backing a session is fine.
	ok := AgentSpec{
		Model:        ModelRef{ProviderRef: "p", Name: "m"},
		Instructions: "x",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 100, MaxWallClockSeconds: 10, MaxToolCalls: 0},
		Session:      &SessionSpec{Required: true},
	}
	if err := ValidateAgent(Agent{Spec: ok}); err != nil {
		t.Errorf("loop Agent with session.required must pass: %v", err)
	}
}

// M2.23: artifacts require agentfs storage, unique names, and relative globs.
func TestValidateAgent_Artifacts(t *testing.T) {
	base := AgentSpec{
		Model:        ModelRef{ProviderRef: "p", Name: "m"},
		Instructions: "x",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 100, MaxWallClockSeconds: 10},
	}
	withFS := base
	withFS.Storage = &StorageSpec{Kind: StorageAgentFS, AgentFS: &AgentFSSpec{SizeGiB: 1}}

	ok := withFS
	ok.Artifacts = &ArtifactSpec{Outputs: []ArtifactRule{{Name: "out", Glob: "out/**/*.json"}}}
	if err := ValidateAgent(Agent{Spec: ok}); err != nil {
		t.Errorf("valid artifacts rejected: %v", err)
	}

	noFS := base
	noFS.Artifacts = &ArtifactSpec{Outputs: []ArtifactRule{{Name: "out", Glob: "o/*"}}}
	if err := ValidateAgent(Agent{Spec: noFS}); err == nil {
		t.Errorf("artifacts without agentfs must be rejected")
	}

	dup := withFS
	dup.Artifacts = &ArtifactSpec{Outputs: []ArtifactRule{{Name: "x", Glob: "a"}, {Name: "x", Glob: "b"}}}
	if err := ValidateAgent(Agent{Spec: dup}); err == nil {
		t.Errorf("duplicate artifact names must be rejected")
	}

	bad := withFS
	bad.Artifacts = &ArtifactSpec{Outputs: []ArtifactRule{{Name: "x", Glob: "../escape"}}}
	if err := ValidateAgent(Agent{Spec: bad}); err == nil {
		t.Errorf("../ traversal glob must be rejected")
	}
}

func TestValidateAgentRun_DecisionToken(t *testing.T) {
	run := AgentRun{Spec: AgentRunSpec{AgentRef: "a", Input: json.RawMessage(`{}`)}}
	run.Spec.Decision = &Decision{Approve: true} // no token
	if err := ValidateAgentRun(run); err == nil {
		t.Errorf("decision without token must be rejected")
	}
	run.Spec.Decision = &Decision{Token: "t", Approve: true}
	if err := ValidateAgentRun(run); err != nil {
		t.Errorf("decision with token must pass: %v", err)
	}
}

func TestValidateTool_HappyPath(t *testing.T) {
	tool := Tool{
		Name: "search",
		Spec: ToolSpec{
			Kind:         ToolHTTP,
			InputSchema:  goodSchema(),
			OutputSchema: goodSchema(),
			HTTP:         &HTTPSpec{URL: "https://example.com/search"},
		},
	}
	if err := ValidateTool(tool); err != nil {
		t.Errorf("good tool rejected: %v", err)
	}
}

// M3.5: A2A target bounds (maxTokens/timeoutSeconds) reject negative values.
func TestValidateTool_AgentTargetBounds(t *testing.T) {
	mk := func(mt int64, ts int32) Tool {
		return Tool{Name: "deleg", Spec: ToolSpec{
			Kind: ToolAgent, InputSchema: goodSchema(), OutputSchema: goodSchema(),
			Agent: &AgentTargetSpec{Ref: ToolRef{Name: "child"}, MaxTokens: mt, TimeoutSeconds: ts},
		}}
	}
	if err := ValidateTool(mk(1000, 60)); err != nil {
		t.Errorf("valid bounds rejected: %v", err)
	}
	if err := ValidateTool(mk(-1, 0)); err == nil {
		t.Errorf("negative maxTokens must be rejected")
	}
	if err := ValidateTool(mk(0, -5)); err == nil {
		t.Errorf("negative timeoutSeconds must be rejected")
	}
}

func TestValidateTool_KindMismatch(t *testing.T) {
	// kind=mcp but no MCP spec
	tool := Tool{
		Name: "x",
		Spec: ToolSpec{Kind: ToolMCP, InputSchema: goodSchema(), OutputSchema: goodSchema()},
	}
	err := ValidateTool(tool)
	if err == nil || !strings.Contains(err.Error(), "mcp.url") {
		t.Errorf("expected mcp error, got %v", err)
	}
}

func TestValidateTool_BadSchema(t *testing.T) {
	tool := Tool{
		Name: "x",
		Spec: ToolSpec{Kind: ToolHTTP,
			InputSchema:  json.RawMessage(`{}`),
			OutputSchema: goodSchema(),
			HTTP:         &HTTPSpec{URL: "https://x"}},
	}
	if err := ValidateTool(tool); err == nil {
		t.Error("expected schema error for {}")
	}
}

func TestValidateProvider(t *testing.T) {
	good := ModelProvider{Name: "openai", Spec: ModelProviderSpec{Kind: "openai", SecretRef: AuthRef{SecretName: "k"}}}
	if err := ValidateModelProvider(good); err != nil {
		t.Errorf("good rejected: %v", err)
	}
	bad := ModelProvider{Name: "openai"}
	if err := ValidateModelProvider(bad); err == nil {
		t.Error("missing kind not caught")
	}
}

func TestValidateAgentRun(t *testing.T) {
	good := AgentRun{Spec: AgentRunSpec{AgentRef: "a", Input: json.RawMessage(`{"q":"hi"}`)}}
	if err := ValidateAgentRun(good); err != nil {
		t.Errorf("good rejected: %v", err)
	}
	if err := ValidateAgentRun(AgentRun{}); err == nil {
		t.Error("empty run accepted")
	}
}

func TestValidateAgentRun_Inputs(t *testing.T) {
	withInput := func(in RunInputFile) AgentRun {
		return AgentRun{Spec: AgentRunSpec{AgentRef: "a", Input: json.RawMessage(`{"q":"hi"}`), Inputs: []RunInputFile{in}}}
	}
	// Valid: exactly one source, relative path.
	if err := ValidateAgentRun(withInput(RunInputFile{Path: "a/b.txt", Inline: "x"})); err != nil {
		t.Errorf("valid inline input rejected: %v", err)
	}
	if err := ValidateAgentRun(withInput(RunInputFile{Path: "k", SecretRef: &AuthRef{SecretName: "s"}})); err != nil {
		t.Errorf("valid secretRef input rejected: %v", err)
	}
	// Invalid paths (empty, absolute, traversal).
	for _, p := range []string{"", "/abs.txt", "../escape", "a/../../b"} {
		if err := ValidateAgentRun(withInput(RunInputFile{Path: p, Inline: "x"})); err == nil {
			t.Errorf("path %q should be rejected", p)
		}
	}
	// Zero sources / two sources.
	if err := ValidateAgentRun(withInput(RunInputFile{Path: "a"})); err == nil {
		t.Error("input with no source should be rejected")
	}
	if err := ValidateAgentRun(withInput(RunInputFile{Path: "a", Inline: "x", InlineBase64: "eA=="})); err == nil {
		t.Error("input with two sources should be rejected")
	}
}

func TestValidateAgentPolicy(t *testing.T) {
	if err := ValidateAgentPolicy(AgentPolicy{Name: "p"}); err != nil {
		t.Errorf("minimal policy rejected: %v", err)
	}
	bad := AgentPolicy{Name: "p", Spec: AgentPolicySpec{MaxBudget: &Budget{MaxSteps: 0}}}
	if err := ValidateAgentPolicy(bad); err == nil {
		t.Error("bad budget on policy accepted")
	}
}

func TestValidateJSONSchemaShape(t *testing.T) {
	good := []json.RawMessage{
		json.RawMessage(`{"type":"object"}`),
		json.RawMessage(`{"$ref":"#/foo"}`),
		json.RawMessage(`{"oneOf":[{"type":"string"}]}`),
		json.RawMessage(`{"anyOf":[{"type":"string"}]}`),
	}
	for _, s := range good {
		if err := ValidateJSONSchemaShape(s); err != nil {
			t.Errorf("good schema rejected: %s — %v", s, err)
		}
	}
	bad := []json.RawMessage{
		nil,
		json.RawMessage(`{}`),
		json.RawMessage(`not json`),
	}
	for _, s := range bad {
		if err := ValidateJSONSchemaShape(s); err == nil {
			t.Errorf("bad schema accepted: %s", s)
		}
	}
}

func TestMatchesSchema(t *testing.T) {
	if err := MatchesSchema(goodSchema(), json.RawMessage(`{"x":1}`)); err != nil {
		t.Errorf("good match rejected: %v", err)
	}
	if err := MatchesSchema(goodSchema(), json.RawMessage(`not json`)); err == nil {
		t.Error("bad value accepted")
	}
}
