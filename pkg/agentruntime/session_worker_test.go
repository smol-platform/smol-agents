package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
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
	n, err := w.processTurns(context.Background(), state)
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

// M2.18: at width >1 the worker processes turns concurrently (bounded by
// MaxConcurrentTurns) yet folds each exactly once — unique indices, TotalTurns
// == #turns — with no data races (run under -race). The fake runTurn records
// observed concurrency to prove parallelism actually happened and stayed capped.
func TestSessionWorker_ConcurrentTurns(t *testing.T) {
	ws := t.TempDir()
	const n, width = 12, 4
	var inflight, maxSeen int64
	run := func(_ context.Context, _ v1.Agent, turn v1.AgentRunSpec) (Result, error) {
		cur := atomic.AddInt64(&inflight, 1)
		for { // record the high-water mark of concurrent invocations
			m := atomic.LoadInt64(&maxSeen)
			if cur <= m || atomic.CompareAndSwapInt64(&maxSeen, m, cur) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt64(&inflight, -1)
		return Result{Phase: v1.PhaseCompleted, Output: turn.Input, Usage: v1.Usage{Tokens: 1}}, nil
	}
	w := &SessionWorker{Agent: v1.Agent{}, Workspace: ws, run: run, Now: time.Now, MaxConcurrentTurns: width}
	for i := 0; i < n; i++ {
		dropTurn(t, ws, fmt.Sprintf("%04d.json", i), `{"prompt":"x"}`)
	}

	state := &SessionState{}
	got, err := w.processTurns(context.Background(), state)
	if err != nil || got != n {
		t.Fatalf("processTurns = (%d,%v), want (%d,nil)", got, err, n)
	}
	if state.TotalTurns != n || len(state.Turns) != n {
		t.Fatalf("TotalTurns=%d len=%d, want %d/%d", state.TotalTurns, len(state.Turns), n, n)
	}
	seen := map[int]int{}
	for _, tn := range state.Turns {
		seen[tn.Index]++
	}
	for i := 0; i < n; i++ {
		if seen[i] != 1 {
			t.Errorf("index %d folded %d times, want exactly 1", i, seen[i])
		}
	}
	if maxSeen < 2 {
		t.Errorf("no concurrency observed (maxSeen=%d) — width not effective", maxSeen)
	}
	if maxSeen > width {
		t.Errorf("concurrency %d exceeded width %d", maxSeen, width)
	}
}

// M2.18: HistoryLimit compacts the in-memory log (oldest dropped) while
// TotalTurns stays monotonic, so indices never regress across compaction.
func TestSessionWorker_CompactsHistory(t *testing.T) {
	ws := t.TempDir()
	w := newTestWorker(ws)
	w.HistoryLimit = 3
	const n = 7
	for i := 0; i < n; i++ {
		dropTurn(t, ws, fmt.Sprintf("%04d.json", i), `{"prompt":"x"}`)
	}

	state := &SessionState{}
	if _, err := w.processTurns(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if len(state.Turns) != 3 {
		t.Fatalf("retained %d turns, want 3 (compacted to HistoryLimit)", len(state.Turns))
	}
	if state.TotalTurns != n {
		t.Fatalf("TotalTurns=%d, want %d (monotonic across compaction)", state.TotalTurns, n)
	}
	// Serial path is FIFO, so the retained window is the last three (indices 4,5,6).
	if state.Turns[0].Index != 4 || state.Turns[2].Index != 6 {
		t.Errorf("retained indices = %d..%d, want 4..6", state.Turns[0].Index, state.Turns[2].Index)
	}
}

// M2.18: TurnTimeout bounds a single turn's wall-clock via the turn context, so
// a stuck turn is cancelled rather than holding a slot forever.
func TestSessionWorker_TurnTimeout(t *testing.T) {
	ws := t.TempDir()
	var sawDeadline int64
	run := func(ctx context.Context, _ v1.Agent, _ v1.AgentRunSpec) (Result, error) {
		if _, ok := ctx.Deadline(); ok {
			atomic.StoreInt64(&sawDeadline, 1)
		}
		select {
		case <-ctx.Done():
			return Result{Phase: v1.PhaseExpired, TerminationReason: "ctx:deadline"}, ctx.Err()
		case <-time.After(2 * time.Second):
			return Result{Phase: v1.PhaseCompleted}, nil
		}
	}
	w := &SessionWorker{Agent: v1.Agent{}, Workspace: ws, run: run, Now: time.Now, TurnTimeout: 20 * time.Millisecond}
	dropTurn(t, ws, "0001.json", `{"prompt":"x"}`)

	state := &SessionState{}
	start := time.Now()
	if _, err := w.processTurns(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("turn not bounded by TurnTimeout: took %v", elapsed)
	}
	if atomic.LoadInt64(&sawDeadline) != 1 {
		t.Error("turn context carried no deadline; TurnTimeout not applied")
	}
	if len(state.Turns) != 1 || state.Turns[0].Phase != v1.PhaseExpired {
		t.Fatalf("expected one Expired turn, got %+v", state.Turns)
	}
}

// M2.19: the worker checkpoints a status-summary.json beside the full state so
// the operator can mirror it into AgentSession.status. Drive Run to idle-exit so
// it exercises the real checkpoint path (not just the helper).
func TestSessionWorker_WritesStatusSummary(t *testing.T) {
	ws := t.TempDir()
	w := newTestWorker(ws)
	w.PollInterval = time.Millisecond
	w.IdleTimeout = 10 * time.Millisecond
	dropTurn(t, ws, "0001.json", `{"prompt":"one"}`)
	dropTurn(t, ws, "0002.json", `{"prompt":"two"}`)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run (idle-exit) = %v, want nil", err)
	}

	b, err := os.ReadFile(DefaultSessionSummaryPath(ws))
	if err != nil {
		t.Fatalf("status-summary.json not written: %v", err)
	}
	var sum SessionSummary
	if err := json.Unmarshal(b, &sum); err != nil {
		t.Fatalf("summary decode: %v", err)
	}
	if sum.Turns != 2 || sum.Usage.Tokens != 14 {
		t.Errorf("summary turns=%d tokens=%d, want 2/14", sum.Turns, sum.Usage.Tokens)
	}
	if sum.LastTurnTime == nil {
		t.Error("summary LastTurnTime nil after two turns")
	}
}

func TestSessionWorker_ResumeFromCheckpoint(t *testing.T) {
	ws := t.TempDir()
	w := newTestWorker(ws)
	store := w.store()

	// Turn 1, then checkpoint.
	dropTurn(t, ws, "0001.json", `{"prompt":"one"}`)
	state, _ := store.Load()
	if _, err := w.processTurns(context.Background(), state); err != nil {
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
	if _, err := w2.processTurns(context.Background(), resumed); err != nil {
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
