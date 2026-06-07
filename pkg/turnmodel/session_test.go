package turnmodel

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestSessionState_Append(t *testing.T) {
	s := &SessionState{Phase: v1.PhaseRunning}
	now := time.Unix(1000, 0)
	s.Append(SessionTurn{Usage: v1.Usage{Steps: 1, Tokens: 100, ToolCalls: 2}}, now)
	s.Append(SessionTurn{Usage: v1.Usage{Steps: 2, Tokens: 50, ToolCalls: 1}}, now.Add(time.Minute))

	if len(s.Turns) != 2 || s.Turns[0].Index != 0 || s.Turns[1].Index != 1 {
		t.Fatalf("turn indices not assigned in order: %+v", s.Turns)
	}
	if got := s.CumulativeUsage; got.Steps != 3 || got.Tokens != 150 || got.ToolCalls != 3 {
		t.Errorf("cumulative usage = %+v, want steps=3 tokens=150 toolCalls=3", got)
	}
	if !s.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Errorf("UpdatedAt = %v, want last append time", s.UpdatedAt)
	}
}

// M2.18: TotalTurns is monotonic (drives Index so it survives future history
// compaction), and FailedTurns counts turns that ended Failed or carried an
// error — both independent of how many turns remain in the in-memory log.
func TestSessionState_TurnCounters(t *testing.T) {
	s := &SessionState{Phase: v1.PhaseRunning}
	now := time.Unix(2000, 0)
	s.Append(SessionTurn{Phase: v1.PhaseCompleted}, now)
	s.Append(SessionTurn{Phase: v1.PhaseFailed}, now)
	s.Append(SessionTurn{Phase: v1.PhaseCompleted, Error: "tool timeout"}, now)

	if s.TotalTurns != 3 {
		t.Errorf("TotalTurns = %d, want 3", s.TotalTurns)
	}
	if s.FailedTurns != 2 {
		t.Errorf("FailedTurns = %d, want 2 (one Failed phase + one carrying Error)", s.FailedTurns)
	}

	// Simulate compaction dropping the oldest turn: TotalTurns must NOT regress,
	// and the next index must keep climbing past the compacted length.
	s.Turns = s.Turns[1:]
	s.Append(SessionTurn{Phase: v1.PhaseCompleted}, now)
	if s.TotalTurns != 4 {
		t.Errorf("TotalTurns after compaction = %d, want 4 (monotonic)", s.TotalTurns)
	}
	if last := s.Turns[len(s.Turns)-1]; last.Index != 3 {
		t.Errorf("post-compaction turn Index = %d, want 3 (from TotalTurns, not len)", last.Index)
	}
}

// M2.19: Summary mirrors CumulativeUsage verbatim (NOT via Usage.Add), reports
// the monotonic turn count + failures, and points LastTurnTime at the most
// recent retained turn.
func TestSessionState_Summary(t *testing.T) {
	s := &SessionState{Phase: v1.PhaseRunning}
	now := time.Unix(3000, 0)
	s.Append(SessionTurn{Phase: v1.PhaseCompleted, Usage: v1.Usage{Steps: 1, Tokens: 100, ToolCalls: 2}, EndedAt: now}, now)
	s.Append(SessionTurn{Phase: v1.PhaseFailed, Usage: v1.Usage{Steps: 1, Tokens: 5}, EndedAt: now.Add(time.Minute)}, now.Add(time.Minute))

	sum := s.Summary()
	if sum.Usage != s.CumulativeUsage {
		t.Errorf("Summary.Usage = %+v, want CumulativeUsage %+v (field-wise, verbatim)", sum.Usage, s.CumulativeUsage)
	}
	if sum.Turns != 2 || sum.FailedTurns != 1 {
		t.Errorf("Summary turns=%d failed=%d, want 2/1", sum.Turns, sum.FailedTurns)
	}
	if sum.Phase != v1.PhaseRunning {
		t.Errorf("Summary.Phase = %q, want Running", sum.Phase)
	}
	if sum.LastTurnTime == nil || !sum.LastTurnTime.Equal(now.Add(time.Minute)) {
		t.Errorf("Summary.LastTurnTime = %v, want last turn EndedAt %v", sum.LastTurnTime, now.Add(time.Minute))
	}

	// A fresh, turnless session has no LastTurnTime.
	if (&SessionState{}).Summary().LastTurnTime != nil {
		t.Error("turnless session must have nil LastTurnTime")
	}
}

func TestSessionStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work", ".smol-session", "state.json")
	st := SessionStore{Path: path}

	orig := &SessionState{
		AgentRef: "a1", Phase: v1.PhaseRequiresAction,
		Turns: []SessionTurn{{
			Index: 0, Phase: v1.PhaseCompleted,
			Input: json.RawMessage(`{"prompt":"hi"}`), Output: json.RawMessage(`"done"`),
			Usage: v1.Usage{Steps: 1, Tokens: 42},
		}},
		CumulativeUsage: v1.Usage{Steps: 1, Tokens: 42},
		CreatedAt:       time.Unix(100, 0).UTC(),
		UpdatedAt:       time.Unix(200, 0).UTC(),
	}
	if err := st.Save(orig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AgentRef != "a1" || got.Phase != v1.PhaseRequiresAction || len(got.Turns) != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if string(got.Turns[0].Output) != `"done"` || got.CumulativeUsage.Tokens != 42 {
		t.Errorf("turn/usage not preserved: %+v", got.Turns[0])
	}
}

func TestSessionStore_LoadMissingIsFresh(t *testing.T) {
	st := SessionStore{Path: filepath.Join(t.TempDir(), "nope", "state.json")}
	s, err := st.Load()
	if err != nil {
		t.Fatalf("Load of missing checkpoint must not error: %v", err)
	}
	if s.Phase != v1.PhasePending || len(s.Turns) != 0 {
		t.Errorf("missing checkpoint must yield a fresh Pending state, got %+v", s)
	}
}
