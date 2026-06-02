package agentruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// SessionWorker is the long-running runtime behind an AgentSession. It restores
// durable state (the turn log via SessionStore + the agent's files via the
// AgentFS init container), then loops: process turns dropped into its inbox,
// checkpoint after each, and park in RequiresAction when idle — until ctx is
// cancelled (SIGTERM), where it writes a final checkpoint so nothing is lost.
//
// Turn DELIVERY is intentionally out of scope: Phase 3 reads turns from an
// on-disk inbox under the workspace; the gateway/NATS path (Phase 4) writes
// turns there. That keeps the durable-execution core independent of transport.
type SessionWorker struct {
	Agent        v1.Agent
	AgentRef     string // the Agent CR name, recorded in checkpoint metadata
	Workspace    string // AgentFS mount; state + inbox/outbox live under it
	Leaser       SecretLeaser
	LLM          LLM
	PollInterval time.Duration // inbox poll cadence (default 2s)
	IdleTimeout  time.Duration // exit (for scale-to-zero) after this idle; 0 = never
	Now          func() time.Time
	Logger       *slog.Logger

	// run executes one turn; nil defaults to RunTurn. Injected by tests.
	run func(context.Context, v1.Agent, v1.AgentRunSpec) (Result, error)
}

func (w *SessionWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *SessionWorker) poll() time.Duration {
	if w.PollInterval > 0 {
		return w.PollInterval
	}
	return 2 * time.Second
}

func (w *SessionWorker) store() SessionStore {
	return SessionStore{Path: DefaultSessionStatePath(w.Workspace)}
}

func (w *SessionWorker) inboxDir() string {
	return filepath.Join(w.Workspace, ".smol-session", "inbox")
}

func (w *SessionWorker) outboxDir() string {
	return filepath.Join(w.Workspace, ".smol-session", "outbox")
}

func (w *SessionWorker) log(msg string, args ...any) {
	if w.Logger != nil {
		w.Logger.Info(msg, args...)
	}
}

func (w *SessionWorker) runTurn(ctx context.Context, turn v1.AgentRunSpec) (Result, error) {
	if w.run != nil {
		return w.run(ctx, w.Agent, turn)
	}
	return RunTurn(ctx, w.Agent, turn, w.Leaser, w.LLM)
}

// Run drives the session until ctx is cancelled, returning ctx.Err() on a
// SIGTERM-style stop (a clean shutdown to the caller) or nil on idle timeout. A
// final checkpoint is always written via defer.
func (w *SessionWorker) Run(ctx context.Context) error {
	store := w.store()
	state, err := store.Load()
	if err != nil {
		return err
	}
	if state.CreatedAt.IsZero() {
		state.CreatedAt = w.now()
	}
	if w.AgentRef != "" {
		state.AgentRef = w.AgentRef
	}
	state.Phase = v1.PhaseRunning
	_ = store.Save(state)
	defer func() { _ = store.Save(state) }()

	idleSince := w.now()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, perr := w.processInbox(ctx, state)
		if perr != nil {
			w.log("session inbox error", "err", perr)
		}
		if err := store.Save(state); err != nil {
			w.log("checkpoint error", "err", err)
		}
		if n > 0 {
			idleSince = w.now()
			continue
		}
		// Idle: park awaiting the next turn.
		if state.Phase != v1.PhaseRequiresAction {
			state.Phase = v1.PhaseRequiresAction
			_ = store.Save(state)
		}
		if w.IdleTimeout > 0 && w.now().Sub(idleSince) >= w.IdleTimeout {
			w.log("session idle timeout; exiting for scale-to-zero", "turns", len(state.Turns))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.poll()):
		}
	}
}

// processInbox runs every pending turn file (oldest first by name), appending
// each result to state and writing it to the outbox. Returns the count handled.
func (w *SessionWorker) processInbox(ctx context.Context, state *SessionState) (int, error) {
	entries, err := os.ReadDir(w.inboxDir())
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // lexical; callers timestamp-prefix turn files for FIFO
	handled := 0
	for _, name := range names {
		if ctx.Err() != nil {
			return handled, ctx.Err()
		}
		if err := w.handleTurnFile(ctx, state, name); err != nil {
			w.log("turn error", "file", name, "err", err)
		}
		handled++
	}
	return handled, nil
}

func (w *SessionWorker) handleTurnFile(ctx context.Context, state *SessionState, name string) error {
	path := filepath.Join(w.inboxDir(), name)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var turn v1.AgentRunSpec
	if err := json.Unmarshal(b, &turn); err != nil {
		_ = os.Remove(path) // bad turn: ack so it can't wedge the queue
		return fmt.Errorf("decode turn %q: %w", name, err)
	}

	state.Phase = v1.PhaseRunning
	started := w.now()
	res, runErr := w.runTurn(ctx, turn)
	st := SessionTurn{
		Input:             turn.Input,
		Output:            res.Output,
		Phase:             res.Phase,
		Usage:             res.Usage,
		TerminationReason: res.TerminationReason,
		StartedAt:         started,
		EndedAt:           w.now(),
	}
	if runErr != nil {
		st.Error = runErr.Error()
		if st.Phase == "" {
			st.Phase = v1.PhaseFailed
		}
	}
	state.Append(st, w.now())

	w.writeOutbox(name, st) // best-effort result for the caller/gateway
	return os.Remove(path)  // ack: processed exactly once
}

func (w *SessionWorker) writeOutbox(name string, st SessionTurn) {
	if err := os.MkdirAll(w.outboxDir(), 0o700); err != nil {
		w.log("outbox mkdir", "err", err)
		return
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(w.outboxDir(), name), b, 0o600)
}
