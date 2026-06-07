package turnmodel

import (
	"encoding/json"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func hermesAgent() v1.Agent {
	return v1.Agent{Spec: v1.AgentSpec{Mode: v1.ModeHarness, Harness: &v1.HarnessSpec{Kind: v1.HarnessHermes}}}
}
func cliAgent() v1.Agent {
	return v1.Agent{Spec: v1.AgentSpec{Mode: v1.ModeHarness, Harness: &v1.HarnessSpec{Kind: v1.HarnessClaudeCode}}}
}
func loopAgent() v1.Agent { return v1.Agent{Spec: v1.AgentSpec{Mode: v1.ModeLoop}} }

// M4.2: the strategy is derived from the runtime kind — Hermes carries memory
// provider-side, CLI on the workspace, loop replays (the deferred stub).
func TestSelectMemoryStrategy(t *testing.T) {
	cases := []struct {
		name  string
		agent v1.Agent
		want  MemoryStrategy
	}{
		{"hermes", hermesAgent(), MemoryProviderSession},
		{"cli", cliAgent(), MemoryWorkspaceOnly},
		{"loop", loopAgent(), MemoryHistoryReplay},
		{"loop-default-mode", v1.Agent{}, MemoryHistoryReplay}, // empty mode == loop
	}
	for _, c := range cases {
		if got := SelectMemoryStrategy(c.agent); got != c.want {
			t.Errorf("%s: SelectMemoryStrategy = %q, want %q", c.name, got, c.want)
		}
	}
}

// M4.2: a Hermes session carries a STABLE provider session id across N turns —
// the property that makes its gateway-side memory accumulate rather than reset.
func TestBuildMemory_HermesStableAcrossTurns(t *testing.T) {
	w := &SessionWorker{Agent: hermesAgent(), AgentRef: "sess-7"}
	state := &SessionState{}
	for turn := 0; turn < 3; turn++ {
		m := w.buildMemory(state, w.AgentRef)
		if m.ProviderSessionID != "sess-7" {
			t.Fatalf("turn %d: ProviderSessionID = %q, want stable %q", turn, m.ProviderSessionID, "sess-7")
		}
		if m.PriorOutput != nil || m.History != nil {
			t.Errorf("turn %d: provider-session must carry no in-band memory, got %+v", turn, m)
		}
		state.Append(SessionTurn{Output: json.RawMessage(`"x"`)}, state.UpdatedAt)
	}
}

// M4.2: a CLI session carries NO in-band memory — its continuity is the AgentFS
// workspace, which persists by itself.
func TestBuildMemory_CLIEmpty(t *testing.T) {
	w := &SessionWorker{Agent: cliAgent(), AgentRef: "sess-cli"}
	state := &SessionState{}
	state.Append(SessionTurn{Output: json.RawMessage(`"prior"`)}, state.UpdatedAt)
	m := w.buildMemory(state, w.AgentRef)
	if m.ProviderSessionID != "" || m.PriorOutput != nil || m.History != nil {
		t.Errorf("CLI memory must be empty (workspace-only), got %+v", m)
	}
}

// M4.2: the loop history-replay path is a D6-deferred, feature-flagged stub —
// OFF by default (independent turns), and when ReplayHistory is set it carries
// the prior turns + the last output (a snapshot, not the live slice).
func TestBuildMemory_LoopHistoryReplayFlag(t *testing.T) {
	state := &SessionState{}
	state.Append(SessionTurn{Output: json.RawMessage(`"first"`)}, state.UpdatedAt)
	state.Append(SessionTurn{Output: json.RawMessage(`"second"`)}, state.UpdatedAt)

	off := &SessionWorker{Agent: loopAgent()}
	if m := off.buildMemory(state, ""); m.PriorOutput != nil || m.History != nil {
		t.Errorf("loop replay must be OFF by default (D6), got %+v", m)
	}

	on := &SessionWorker{Agent: loopAgent(), ReplayHistory: true}
	m := on.buildMemory(state, "")
	if len(m.History) != 2 {
		t.Fatalf("ReplayHistory on: History len = %d, want 2", len(m.History))
	}
	if string(m.PriorOutput) != `"second"` {
		t.Errorf("PriorOutput = %s, want last turn output", m.PriorOutput)
	}
	// Snapshot, not alias: mutating state after must not change the captured memory.
	state.Append(SessionTurn{Output: json.RawMessage(`"third"`)}, state.UpdatedAt)
	if len(m.History) != 2 {
		t.Errorf("memory History must be a snapshot, mutated to len %d", len(m.History))
	}
}
