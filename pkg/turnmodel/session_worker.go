package turnmodel

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	rt "github.com/smol-platform/smol-agents/pkg/agentruntime"
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
	Leaser       rt.SecretLeaser
	LLM          rt.LLM
	PollInterval time.Duration // inbox poll cadence (default 2s)
	IdleTimeout  time.Duration // exit (for scale-to-zero) after this idle; 0 = never
	Now          func() time.Time
	Logger       *slog.Logger

	// Source/Sink select the turn transport. Default: an on-disk inbox/outbox
	// under Workspace (Phase 3). The queue-backed impls (QueueSource/QueueSink)
	// deliver turns + results over NATS for the gateway path (Phase 4).
	Source TurnSource
	Sink   ResultSink

	// MaxConcurrentTurns is the turn-processing width. 0/1 (default) keeps the
	// serial, FIFO-ordered path identical to a single-turn-at-a-time worker; >1
	// opts into bounded concurrency (M2.18) — FIFO is then NOT guaranteed.
	MaxConcurrentTurns int
	// TurnTimeout caps a single turn's wall-clock so one stuck turn can't hold a
	// concurrency slot (or the serial loop) forever. 0 = no per-turn cap (the
	// turn's own budget still applies). Effective deadline is min(TurnTimeout,
	// budget) since the executor enforces the budget independently.
	TurnTimeout time.Duration
	// HistoryLimit bounds the in-memory turn log; turns beyond it are compacted
	// (oldest dropped) after each Append. 0 = unbounded. TotalTurns stays
	// monotonic across compaction, so indices and status counts never regress.
	HistoryLimit int

	// ReplayHistory opts a loop (history-replay) session into carrying prior
	// turns into each Turn's Memory (M4.2). Default false: per D6 the loop-resume
	// engine is deferred, so loop turns are independent. Hermes/CLI ignore it
	// (they carry memory provider-side / on the workspace).
	ReplayHistory bool

	// mu guards the shared SessionState mutations (phase/index/Append/compact)
	// when MaxConcurrentTurns > 1. runTurn itself runs OUTSIDE the lock.
	mu sync.Mutex

	// Executor runs one turn (the Turn-Model → Runtime seam, M4.1). nil defaults
	// to a RuntimeExecutor built from Leaser+LLM (the production RunTurn path);
	// tests inject a fake (ExecutorFunc) so the worker needs no real LLM/harness.
	Executor TurnExecutor
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

// writeSummary checkpoints the status-shaped projection beside the full state so
// the operator can mirror it into AgentSession.status (M2.19) without RBAC for,
// or the cost of reading, the whole turn log. Best-effort: a summary write
// failure never blocks the durable turn loop (the full state is the source of
// truth and was already saved).
func (w *SessionWorker) writeSummary(state *SessionState) {
	path := DefaultSessionSummaryPath(w.Workspace)
	b, err := json.MarshalIndent(state.Summary(), "", "  ")
	if err != nil {
		w.log("session summary marshal", "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		w.log("session summary write", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		w.log("session summary rename", "err", err)
	}
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

// executor returns the worker's TurnExecutor, defaulting to a RuntimeExecutor
// (the production RunTurn path) built from the worker's Leaser + LLM.
func (w *SessionWorker) executor() TurnExecutor {
	if w.Executor != nil {
		return w.Executor
	}
	return RuntimeExecutor{Leaser: w.Leaser, LLM: w.LLM}
}

func (w *SessionWorker) runTurn(ctx context.Context, turn v1.AgentRunSpec, mem TurnMemory) (rt.Result, error) {
	return w.executor().Execute(ctx, Turn{Agent: w.Agent, Spec: turn, Memory: mem})
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
	w.writeSummary(state)
	defer func() { _ = store.Save(state); w.writeSummary(state) }()

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
		w.writeSummary(state)
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

// width is the turn-processing concurrency; 1 (the default) is the serial path.
func (w *SessionWorker) width() int {
	if w.MaxConcurrentTurns > 1 {
		return w.MaxConcurrentTurns
	}
	return 1
}

// processTurns runs every pending turn from the source, appending each result
// to state, publishing it to the sink, and acking it. Returns the count handled.
// At width 1 (default) turns run serially in source order (FIFO preserved). At
// width >1 they run under a bounded-concurrency semaphore (FIFO not guaranteed);
// shared state is mutated only under w.mu, runTurn always outside it.
func (w *SessionWorker) processTurns(ctx context.Context, state *SessionState) (int, error) {
	turns, err := w.source().Poll(ctx)
	if err != nil {
		return 0, err
	}
	if len(turns) == 0 {
		return 0, nil
	}
	if w.width() == 1 {
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

	sem := make(chan struct{}, w.width())
	var wg sync.WaitGroup
	var handled int64
	for _, t := range turns {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(t InboundTurn) {
			defer wg.Done()
			defer func() { <-sem }()
			w.handleTurn(ctx, state, t)
			atomic.AddInt64(&handled, 1)
		}(t)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return int(handled), ctx.Err()
	}
	return int(handled), nil
}

func (w *SessionWorker) handleTurn(ctx context.Context, state *SessionState, t InboundTurn) {
	turnCtx := ctx
	if w.TurnTimeout > 0 {
		var cancel context.CancelFunc
		turnCtx, cancel = context.WithTimeout(ctx, w.TurnTimeout)
		defer cancel()
	}
	started := w.now()
	// Decide what cross-turn memory this turn carries (M4.2), snapshotting the
	// live state under the lock; runTurn itself stays OUTSIDE the lock.
	w.mu.Lock()
	mem := w.buildMemory(state, w.AgentRef)
	w.mu.Unlock()
	res, runErr := w.runTurn(turnCtx, t.Spec, mem) // OUTSIDE the lock — the expensive part
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
	// Take the lock only to mutate shared state. Stamp the index from the
	// monotonic TotalTurns BEFORE Append (Append assigns the same value to the
	// stored copy) so the published result carries the same, compaction-stable
	// index. compact runs here too, under the lock.
	w.mu.Lock()
	state.Phase = v1.PhaseRunning
	st.Index = state.TotalTurns
	state.Append(st, w.now())
	w.compact(state)
	w.mu.Unlock()

	if err := w.sink().Publish(ctx, t.ID, st); err != nil {
		w.log("publish result", "turn", t.ID, "err", err)
	}
	if t.Ack != nil {
		if err := t.Ack(); err != nil {
			w.log("ack turn", "turn", t.ID, "err", err)
		}
	}
}

// compact drops the oldest turns beyond HistoryLimit from the in-memory log,
// copying into a right-sized slice so the dropped turns (and the old backing
// array) are released. TotalTurns is untouched, so indices/status stay
// monotonic. Caller holds w.mu. No-op when HistoryLimit is 0 or not exceeded.
func (w *SessionWorker) compact(state *SessionState) {
	if w.HistoryLimit <= 0 || len(state.Turns) <= w.HistoryLimit {
		return
	}
	keep := make([]SessionTurn, w.HistoryLimit)
	copy(keep, state.Turns[len(state.Turns)-w.HistoryLimit:])
	state.Turns = keep
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
