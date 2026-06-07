package turnmodel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

type fakeGate struct {
	proceed  bool
	edited   string
	reason   string
	err      error
	called   int
	gotInput string
}

func (g *fakeGate) ReviewTurn(_ context.Context, spec v1.AgentRunSpec, _ TurnMemory) (ReviewDecision, error) {
	g.called++
	g.gotInput = string(spec.Input)
	if g.err != nil {
		return ReviewDecision{}, g.err
	}
	d := ReviewDecision{Proceed: g.proceed, Reason: g.reason}
	if g.edited != "" {
		d.EditedInput = json.RawMessage(g.edited)
	}
	return d, nil
}

func TestSessionWorker_ReviewGate_ApproveWithEdit(t *testing.T) {
	ws := t.TempDir()
	w := newTestWorker(ws)
	g := &fakeGate{proceed: true, edited: `{"prompt":"EDITED"}`}
	w.ReviewGate = g
	dropTurn(t, ws, "0001.json", `{"prompt":"orig"}`)

	state := &SessionState{}
	if _, err := w.processTurns(context.Background(), state); err != nil {
		t.Fatalf("processTurns: %v", err)
	}
	if g.called != 1 || g.gotInput != `{"prompt":"orig"}` {
		t.Fatalf("gate should see the original input once: called=%d input=%q", g.called, g.gotInput)
	}
	if len(state.Turns) != 1 || state.Turns[0].Phase != v1.PhaseCompleted {
		t.Fatalf("approved turn should complete: %+v", state.Turns)
	}
	// echoRun echoes the (edited) input → output reflects the human edit.
	if string(state.Turns[0].Output) != `{"prompt":"EDITED"}` {
		t.Fatalf("edited input not applied: %s", state.Turns[0].Output)
	}
}

func TestSessionWorker_ReviewGate_Deny(t *testing.T) {
	ws := t.TempDir()
	w := newTestWorker(ws)
	w.ReviewGate = &fakeGate{proceed: false, reason: "off-topic"}
	dropTurn(t, ws, "0001.json", `{"prompt":"x"}`)

	state := &SessionState{}
	if _, err := w.processTurns(context.Background(), state); err != nil {
		t.Fatalf("processTurns: %v", err)
	}
	if len(state.Turns) != 1 || state.Turns[0].Phase != v1.PhaseCancelled {
		t.Fatalf("denied turn should be Cancelled: %+v", state.Turns)
	}
	if !strings.HasPrefix(state.Turns[0].TerminationReason, "review:denied") {
		t.Fatalf("reason: %q", state.Turns[0].TerminationReason)
	}
	// A denied turn never ran: no usage accrued.
	if state.CumulativeUsage.Tokens != 0 {
		t.Fatalf("denied turn must not consume tokens, got %d", state.CumulativeUsage.Tokens)
	}
}

func TestSessionWorker_ReviewGate_ErrorFailsClosed(t *testing.T) {
	ws := t.TempDir()
	w := newTestWorker(ws)
	w.ReviewGate = &fakeGate{err: context.DeadlineExceeded}
	dropTurn(t, ws, "0001.json", `{"prompt":"x"}`)

	state := &SessionState{}
	if _, err := w.processTurns(context.Background(), state); err != nil {
		t.Fatalf("processTurns: %v", err)
	}
	if len(state.Turns) != 1 || state.Turns[0].Phase != v1.PhaseCancelled {
		t.Fatalf("gate error must fail closed (Cancelled): %+v", state.Turns)
	}
}
