package agentmodel

import (
	"strings"
	"testing"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// TestSessionWorkerCommand covers the M2.18 "controller renders them" seam: the
// turn-scaling knobs reach the serve-session command, and a default session's
// command stays identical to before (knobs are opt-in).
func TestSessionWorkerCommand(t *testing.T) {
	base := []string{"/agent", "serve-session", "--dir=" + builders.RunSpecMountPath, "--agent-ref=sess-agent"}

	t.Run("defaults render only the base command", func(t *testing.T) {
		s := &amv1.AgentSession{Spec: pure.AgentSessionSpec{AgentRef: "sess-agent"}}
		got := sessionWorkerCommand(s)
		if strings.Join(got, " ") != strings.Join(base, " ") {
			t.Errorf("default command = %v, want %v", got, base)
		}
	})

	t.Run("all knobs render their flags", func(t *testing.T) {
		s := &amv1.AgentSession{Spec: pure.AgentSessionSpec{
			AgentRef:                   "sess-agent",
			IdleTimeoutSeconds:         60,
			MaxConcurrentTurns:         4,
			TurnHistoryLimit:           20,
			TurnDeliveryTimeoutSeconds: 90,
		}}
		got := strings.Join(sessionWorkerCommand(s), " ")
		for _, want := range []string{
			"--idle-timeout=60s",
			"--max-concurrent-turns=4",
			"--history-limit=20",
			"--turn-timeout=90s",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("command %q missing %q", got, want)
			}
		}
	})

	t.Run("serial width and zero per-turn timeout are not rendered", func(t *testing.T) {
		// width 1 (serial) and a 0 timeout must stay off the command line so a
		// default session's behavior is preserved.
		s := &amv1.AgentSession{Spec: pure.AgentSessionSpec{
			AgentRef:                   "sess-agent",
			MaxConcurrentTurns:         1,
			TurnDeliveryTimeoutSeconds: 0,
		}}
		got := strings.Join(sessionWorkerCommand(s), " ")
		if strings.Contains(got, "--max-concurrent-turns") || strings.Contains(got, "--turn-timeout") {
			t.Errorf("serial/zero knobs leaked into command: %q", got)
		}
	})
}
