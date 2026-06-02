package agentruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// echoRun is a fake turn runner: it completes immediately, echoing the input as
// output and reporting fixed usage — so worker tests need no real LLM/harness.
func echoRun(_ context.Context, _ v1.Agent, turn v1.AgentRunSpec) (Result, error) {
	return Result{Phase: v1.PhaseCompleted, Output: turn.Input, Usage: v1.Usage{Steps: 1, Tokens: 7}}, nil
}

func dropTurn(t *testing.T, workspace, name string, input string) {
	t.Helper()
	dir := filepath.Join(workspace, ".smol-session", "inbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := v1.AgentRunSpec{Input: json.RawMessage(input)}
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestWorker(workspace string) *SessionWorker {
	return &SessionWorker{Agent: v1.Agent{}, AgentRef: "sess-agent", Workspace: workspace, run: echoRun, Now: time.Now}
}

func TestSessionWorker_ProcessInbox(t *testing.T) {
	ws := t.TempDir()
	w := newTestWorker(ws)
	dropTurn(t, ws, "0001.json", `{"prompt":"one"}`)
	dropTurn(t, ws, "0002.json", `{"prompt":"two"}`)

	state := &SessionState{}
	n, err := w.processInbox(context.Background(), state)
	if err != nil || n != 2 {
		t.Fatalf("processInbox = (%d,%v), want (2,nil)", n, err)
	}
	if len(state.Turns) != 2 || state.CumulativeUsage.Tokens != 14 {
		t.Fatalf("state not accumulated: turns=%d tokens=%d", len(state.Turns), state.CumulativeUsage.Tokens)
	}
	// Inbox files acked (removed); outbox results written.
	for _, name := range []string{"0001.json", "0002.json"} {
		if _, err := os.Stat(filepath.Join(ws, ".smol-session", "inbox", name)); !os.IsNotExist(err) {
			t.Errorf("inbox %s not acked", name)
		}
		if _, err := os.Stat(filepath.Join(ws, ".smol-session", "outbox", name)); err != nil {
			t.Errorf("outbox %s missing: %v", name, err)
		}
	}
}

func TestSessionWorker_ResumeFromCheckpoint(t *testing.T) {
	ws := t.TempDir()
	w := newTestWorker(ws)
	store := w.store()

	// Turn 1, then checkpoint.
	dropTurn(t, ws, "0001.json", `{"prompt":"one"}`)
	state, _ := store.Load()
	if _, err := w.processInbox(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}

	// Fresh worker + reload: the prior turn must come back (resume).
	w2 := newTestWorker(ws)
	resumed, err := w2.store().Load()
	if err != nil || len(resumed.Turns) != 1 {
		t.Fatalf("resume: turns=%d err=%v, want 1", len(resumed.Turns), err)
	}
	// A second turn continues the same session.
	dropTurn(t, ws, "0002.json", `{"prompt":"two"}`)
	if _, err := w2.processInbox(context.Background(), resumed); err != nil {
		t.Fatal(err)
	}
	if len(resumed.Turns) != 2 || resumed.Turns[1].Index != 1 {
		t.Fatalf("second turn not appended after resume: %+v", resumed.Turns)
	}
}

func TestSessionWorker_RunParksAndIdleExits(t *testing.T) {
	ws := t.TempDir()
	w := newTestWorker(ws)
	w.PollInterval = time.Millisecond
	w.IdleTimeout = time.Millisecond
	dropTurn(t, ws, "0001.json", `{"prompt":"hi"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatalf("Run should exit nil on idle timeout, got %v", err)
	}
	final, _ := w.store().Load()
	if len(final.Turns) != 1 {
		t.Errorf("processed turn not checkpointed: turns=%d", len(final.Turns))
	}
	if final.Phase != v1.PhaseRequiresAction {
		t.Errorf("idle session should be parked in RequiresAction, got %s", final.Phase)
	}
}
