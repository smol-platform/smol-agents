package agentruntime

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
