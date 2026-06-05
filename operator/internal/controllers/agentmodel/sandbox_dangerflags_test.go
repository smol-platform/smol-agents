package agentmodel

import (
	"testing"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// M3.15: danger permission/sandbox flags are admission-refused unless the
// resolved RuntimeClass is a kata microVM. The default safe posture, and danger
// flags ON a microVM, are permitted; danger flags on a shared-kernel class
// (runc/gvisor) are a violation. dangerFlagViolation is the fail-closed gate.
func TestDangerFlagViolation(t *testing.T) {
	cli := func(mode string, extra ...string) *amv1.Agent {
		a := &amv1.Agent{}
		a.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessClaudeCode, CLI: &pure.HarnessCLISpec{ApprovalMode: mode, ExtraFlags: extra}}
		return a
	}
	cases := []struct {
		name      string
		agent     *amv1.Agent
		class     string
		violation bool
	}{
		{"no harness", &amv1.Agent{}, "runc", false},
		{"safe default on runc", cli(""), "runc", false},
		{"safe mode on runc", cli("safe"), "runc", false},
		{"approval=never on runc", cli("never"), "runc", true},
		{"approval=never on gvisor", cli("never"), "gvisor", true},
		{"approval=never on kata-fc", cli("never"), "kata-fc", false},
		{"approval=never on kata-qemu", cli("never"), "kata-qemu", false},
		{"--dangerously-skip-permissions on runc", cli("", "--dangerously-skip-permissions"), "runc", true},
		{"--dangerously-skip-permissions on kata-fc", cli("", "--dangerously-skip-permissions"), "kata-fc", false},
		{"codex danger-full-access on runc", cli("", "--sandbox", "danger-full-access"), "runc", true},
		{"codex --ask-for-approval never on runc", cli("", "--ask-for-approval", "never"), "runc", true},
		{"safe codex --ask-for-approval on-failure on runc", cli("", "--ask-for-approval", "on-failure"), "runc", false},
		{"benign extra flag on runc", cli("", "--verbose"), "runc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dangerFlagViolation(tc.agent, tc.class)
			if (got != "") != tc.violation {
				t.Errorf("dangerFlagViolation(%s) = %q, want violation=%v", tc.class, got, tc.violation)
			}
		})
	}
}
