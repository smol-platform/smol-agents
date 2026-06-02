package agentruntime

import (
	"context"
	"encoding/json"
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

	// Source/Sink select the turn transport. Default: an on-disk inbox/outbox
	// under Workspace (Phase 3). The queue-backed impls (QueueSource/QueueSink)
	// deliver turns + results over NATS for the gateway path (Phase 4).
	Source TurnSource
	Sink   ResultSink

	// run executes one turn; nil defaults to RunTurn. Injected by tests.
	run func(context.Context, v1.Agent, v1.AgentRunSpec) (Result, error)
}

// InboundTurn is one pending turn yielded by a TurnSource. Ack marks it durably
// processed (remove the inbox file / ack the NATS message); nil is a no-op.
type InboundTurn struct {
	ID   string
	Spec v1.AgentRunSpec
	Ack  func() error
}

// TurnSource yields pending turns for the worker. Poll returns promptly with
// whatever is ready (empty when idle).
type TurnSource interface {
	Poll(ctx context.Context) ([]InboundTurn, error)
}

// ResultSink records a processed turn's folded result.
type ResultSink interface {
	Publish(ctx context.Context, turnID string, st SessionTurn) error
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
		n, perr := w.processTurns(ctx, state)
		if perr != nil {
			w.log("session turn-source error", "err", perr)
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

func (w *SessionWorker) source() TurnSource {
	if w.Source != nil {
		return w.Source
	}
	return &inboxSource{dir: w.inboxDir()}
}

func (w *SessionWorker) sink() ResultSink {
	if w.Sink != nil {
		return w.Sink
	}
	return &outboxSink{dir: w.outboxDir()}
}

// processTurns runs every pending turn from the source (oldest first),
// appending each result to state, publishing it to the sink, and acking it.
// Returns the count handled.
func (w *SessionWorker) processTurns(ctx context.Context, state *SessionState) (int, error) {
	turns, err := w.source().Poll(ctx)
	if err != nil {
		return 0, err
	}
	handled := 0
	for _, t := range turns {
		if ctx.Err() != nil {
			return handled, ctx.Err()
		}
		w.handleTurn(ctx, state, t)
		handled++
	}
	return handled, nil
}

func (w *SessionWorker) handleTurn(ctx context.Context, state *SessionState, t InboundTurn) {
	state.Phase = v1.PhaseRunning
	started := w.now()
	res, runErr := w.runTurn(ctx, t.Spec)
	st := SessionTurn{
		Input:             t.Spec.Input,
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
	if err := w.sink().Publish(ctx, t.ID, st); err != nil {
		w.log("publish result", "turn", t.ID, "err", err)
	}
	if t.Ack != nil {
		if err := t.Ack(); err != nil {
			w.log("ack turn", "turn", t.ID, "err", err)
		}
	}
}

// inboxSource is the default on-disk TurnSource: turn files (*.json, lexical
// order) under <workspace>/.smol-session/inbox; Ack removes the file.
type inboxSource struct{ dir string }

func (s *inboxSource) Poll(_ context.Context) ([]InboundTurn, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // lexical; callers timestamp-prefix turn files for FIFO
	out := make([]InboundTurn, 0, len(names))
	for _, name := range names {
		path := filepath.Join(s.dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var spec v1.AgentRunSpec
		if err := json.Unmarshal(b, &spec); err != nil {
			_ = os.Remove(path) // drop a malformed turn so it can't wedge the queue
			continue
		}
		p := path
		out = append(out, InboundTurn{ID: name, Spec: spec, Ack: func() error { return os.Remove(p) }})
	}
	return out, nil
}

// outboxSink is the default on-disk ResultSink: the folded turn result lands at
// <workspace>/.smol-session/outbox/<turnID>.
type outboxSink struct{ dir string }

func (s *outboxSink) Publish(_ context.Context, turnID string, st SessionTurn) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir, turnID), b, 0o600)
}
