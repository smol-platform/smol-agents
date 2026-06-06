package turnmodel

import (
	"encoding/json"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// MemoryStrategy is how a runtime carries memory from one turn into the next.
// It is a Turn-Model policy (M4.2): the layer that owns turns decides what each
// turn remembers, rather than each harness deciding implicitly. The strategy is
// derived from the agent's runtime kind (SelectMemoryStrategy).
type MemoryStrategy string

const (
	// MemoryProviderSession: memory lives provider/gateway-side, keyed by a
	// stable session id (Hermes). The Turn carries that id; the conversation,
	// skills, and tokens accrue at the gateway across the session's N turns
	// (D6). Nothing is replayed in-band.
	MemoryProviderSession MemoryStrategy = "provider-session"
	// MemoryWorkspaceOnly: memory is the agent's files on the AgentFS workspace,
	// which persist across turns by themselves (CLI harnesses — claude/codex/pi).
	// The Turn carries no in-band memory; "what I remember" is "the files I left".
	MemoryWorkspaceOnly MemoryStrategy = "workspace-only"
	// MemoryHistoryReplay: the loop has no provider-side session and no durable
	// transcript, so cross-turn continuity would require replaying prior turns
	// into the next prompt. Per D6 this loop-resume engine is DEFERRED post-GA,
	// so the strategy is a feature-flagged stub: the Turn is populated only when
	// SessionWorker.ReplayHistory is set, and the loop executor consuming it is
	// future work. Default (flag off) = each loop turn is independent.
	MemoryHistoryReplay MemoryStrategy = "history-replay"
)

// TurnMemory is the cross-turn memory carried into one Turn. Which fields are
// populated depends on the agent's MemoryStrategy; an empty value means "no
// in-band memory" (the workspace and/or gateway carry it instead).
type TurnMemory struct {
	// ProviderSessionID is the stable session id a provider-session runtime
	// (Hermes) carries so the gateway scopes memory across turns.
	ProviderSessionID string
	// PriorOutput is the previous turn's output, for history-replay (D6 stub).
	PriorOutput json.RawMessage
	// History is the prior turn log, for history-replay (D6 stub). Bounded by the
	// worker's HistoryLimit (it is the retained in-memory log).
	History []SessionTurn
}

// SelectMemoryStrategy picks the cross-turn memory policy for an agent from its
// runtime kind: Hermes → provider-session, other harnesses → workspace-only,
// loop → history-replay (the deferred, flagged stub).
func SelectMemoryStrategy(a v1.Agent) MemoryStrategy {
	mode := a.Spec.Mode
	if mode == "" {
		mode = v1.ModeLoop
	}
	if mode == v1.ModeHarness && a.Spec.Harness != nil {
		if a.Spec.Harness.Kind == v1.HarnessHermes {
			return MemoryProviderSession
		}
		return MemoryWorkspaceOnly
	}
	return MemoryHistoryReplay
}

// buildMemory constructs the TurnMemory for the next turn from the live session
// state, per the agent's strategy. sessionID is the stable per-session key used
// by provider-session (Hermes). For history-replay it returns empty unless the
// worker opted into ReplayHistory (D6): loop-resume is deferred, so by default a
// loop turn carries no memory.
func (w *SessionWorker) buildMemory(state *SessionState, sessionID string) TurnMemory {
	switch SelectMemoryStrategy(w.Agent) {
	case MemoryProviderSession:
		return TurnMemory{ProviderSessionID: sessionID}
	case MemoryHistoryReplay:
		if !w.ReplayHistory {
			return TurnMemory{} // D6: loop-resume deferred; turns are independent.
		}
		// Snapshot the live log (the caller holds w.mu) so a concurrent
		// Append/compact can't race the Turn the runtime reads outside the lock.
		m := TurnMemory{History: append([]SessionTurn(nil), state.Turns...)}
		if n := len(m.History); n > 0 {
			m.PriorOutput = m.History[n-1].Output
		}
		return m
	default: // MemoryWorkspaceOnly — the files persist; nothing in-band.
		return TurnMemory{}
	}
}
