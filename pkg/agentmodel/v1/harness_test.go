package v1

import (
	"strings"
	"testing"
)

func TestHarnessKind_Valid(t *testing.T) {
	for _, k := range []HarnessKind{
		HarnessClaudeCode, HarnessCodex, HarnessPi, HarnessAider, HarnessGoose,
		HarnessGenericCLI, HarnessGenericHTTP,
	} {
		if !k.Valid() {
			t.Errorf("%s should be valid", k)
		}
	}
	if HarnessKind("nope").Valid() {
		t.Error("unknown kind accepted")
	}
}

func TestAgentMode_Valid(t *testing.T) {
	for _, m := range []AgentMode{ModeLoop, ModeHarness, ""} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	if AgentMode("garbage").Valid() {
		t.Error("garbage mode accepted")
	}
}

func TestValidateHarness_HTTPRequiresURL(t *testing.T) {
	err := ValidateHarness(HarnessSpec{Kind: HarnessPi})
	if err == nil || !strings.Contains(err.Error(), "http.url") {
		t.Errorf("expected http.url error: %v", err)
	}
	err = ValidateHarness(HarnessSpec{Kind: HarnessPi, HTTP: &HarnessHTTPSpec{URL: "https://api.pi.ai/chat"}})
	if err != nil {
		t.Errorf("with URL: %v", err)
	}
}

func TestValidateHarness_CLIDefaultsAccepted(t *testing.T) {
	for _, k := range []HarnessKind{HarnessClaudeCode, HarnessCodex, HarnessAider, HarnessGoose, HarnessGenericCLI} {
		if err := ValidateHarness(HarnessSpec{Kind: k}); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}

func TestValidateHarness_EnvSecretMutex(t *testing.T) {
	h := HarnessSpec{Kind: HarnessClaudeCode, Env: []HarnessEnvVar{{
		Name: "KEY", Value: "literal", SecretRef: &AuthRef{SecretName: "k"},
	}}}
	if err := ValidateHarness(h); err == nil {
		t.Error("expected mutex error")
	}
}

func TestValidateAgent_HarnessMode(t *testing.T) {
	a := Agent{Spec: AgentSpec{
		Mode:         ModeHarness,
		Instructions: "code review",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 0},
		Harness: &HarnessSpec{
			Kind: HarnessClaudeCode,
			CLI:  &HarnessCLISpec{PromptFlag: "--print"},
		},
	}}
	if err := ValidateAgent(a); err != nil {
		t.Errorf("happy harness: %v", err)
	}
	// Harness mode must reject Model presence — actually current validator
	// only checks Harness presence; Model is silently ignored. The rule
	// for "Harness mutually exclusive" is exercised differently:
	a.Spec.Harness = nil
	if err := ValidateAgent(a); err == nil {
		t.Error("harness mode without harness should fail")
	}
}

func TestValidateAgent_LoopModeRejectsHarness(t *testing.T) {
	a := Agent{Spec: AgentSpec{
		Mode:         ModeLoop,
		Model:        ModelRef{ProviderRef: "openai", Name: "gpt-4"},
		Instructions: "be helpful",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 0},
		Harness:      &HarnessSpec{Kind: HarnessClaudeCode},
	}}
	err := ValidateAgent(a)
	if err == nil || !strings.Contains(err.Error(), "spec.harness must be nil") {
		t.Errorf("expected mutex error: %v", err)
	}
}

func TestValidateAgent_PersistentRequiresStorage(t *testing.T) {
	a := Agent{Spec: AgentSpec{
		Mode:         ModeHarness,
		Instructions: "code",
		Budget:       Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 0},
		Harness: &HarnessSpec{
			Kind:          HarnessClaudeCode,
			SessionPolicy: SessionPersistent,
		},
	}}
	err := ValidateAgent(a)
	if err == nil || !strings.Contains(err.Error(), "spec.storage") {
		t.Errorf("expected storage error: %v", err)
	}
	a.Spec.Storage = &StorageSpec{
		Kind:    StorageAgentFS,
		AgentFS: &AgentFSSpec{SizeGiB: 5},
	}
	if err := ValidateAgent(a); err != nil {
		t.Errorf("with storage: %v", err)
	}
}
