package builders

import (
	"strings"
	"testing"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func harnessAgent(kind pure.HarnessKind, image, version string) *amv1.Agent {
	a := &amv1.Agent{}
	a.Spec.Mode = pure.ModeHarness
	a.Spec.Harness = &pure.HarnessSpec{Kind: kind, Image: image, Version: version}
	return a
}

func TestHarnessImage(t *testing.T) {
	cases := []struct {
		name     string
		agent    *amv1.Agent
		contains string // substring the resolved ref must contain
	}{
		{"claude-code default bundle", harnessAgent(pure.HarnessClaudeCode, "", ""), "/harness-claude-code:0.1.0"},
		{"codex default bundle", harnessAgent(pure.HarnessCodex, "", ""), "/harness-codex:0.1.0"},
		{"aider default bundle", harnessAgent(pure.HarnessAider, "", ""), "/harness-aider:0.1.0"},
		{"goose default bundle", harnessAgent(pure.HarnessGoose, "", ""), "/harness-goose:0.1.0"},
		{"version pins the tag", harnessAgent(pure.HarnessClaudeCode, "", "1.2.3"), "/harness-claude-code:1.2.3"},
		{"explicit image wins", harnessAgent(pure.HarnessClaudeCode, "example.com/my/claude:dev", "1.2.3"), "example.com/my/claude:dev"},
		{"hermes (HTTP) uses base agent image", harnessAgent(pure.HarnessHermes, "", ""), "/agent:0.1.0"},
		{"generic-cli without image uses base agent image", harnessAgent(pure.HarnessGenericCLI, "", ""), "/agent:0.1.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HarnessImage(tc.agent)
			if !strings.Contains(got, tc.contains) {
				t.Errorf("HarnessImage = %q, want substring %q", got, tc.contains)
			}
		})
	}

	// Loop mode (no harness) → base agent image.
	loop := &amv1.Agent{}
	loop.Spec.Mode = pure.ModeLoop
	if got := HarnessImage(loop); !strings.Contains(got, "/agent:0.1.0") {
		t.Errorf("loop-mode HarnessImage = %q, want base agent image", got)
	}
}
