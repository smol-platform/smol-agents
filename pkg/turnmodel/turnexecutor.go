// Package turnmodel is the Turn-Model layer: it owns turns, sessions, durable
// state, and turn delivery (SessionWorker, SessionState, TurnSource/ResultSink).
// It executes a turn ONLY through the TurnExecutor seam — the single, explicit
// coupling to the Runtime layer (pkg/agentruntime), which executes exactly one
// turn (executor / harness / pod). The dependency direction is one-way:
// turnmodel → agentruntime, never the reverse (M4.1).
package turnmodel

import (
	"context"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	rt "github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// Turn is one unit of work handed to a TurnExecutor: the agent to run, the
// per-turn spec (input, budget override, resume id, input files), and the
// cross-turn Memory the Turn-Model layer decided this turn should carry (M4.2).
// The Turn-Model layer constructs it; the Runtime layer executes it without
// knowing about sessions, queues, or checkpoints.
type Turn struct {
	Agent  v1.Agent
	Spec   v1.AgentRunSpec
	Memory TurnMemory
}

// Result is a turn's outcome. It is an alias for the runtime's executor Result
// so the worker folds it into a SessionTurn without translation — the seam is
// the interface, not a parallel data model.
type Result = rt.Result

// TurnExecutor executes exactly one turn. It is the sole abstraction the
// Turn-Model layer needs from the Runtime layer: given a Turn, produce a Result.
// RuntimeExecutor is the production implementation (it drives RunTurn); tests
// inject a fake (see ExecutorFunc).
type TurnExecutor interface {
	Execute(ctx context.Context, t Turn) (Result, error)
}

// RuntimeExecutor is the reference TurnExecutor: it runs a turn through the
// agentruntime executor via RunTurn (budget override → input materialization →
// one bounded executor run). It carries the per-run dependencies the runtime
// needs — the secret broker leaser, the loop-mode LLM, and any RunOptions (the
// loop tool catalog + invokers) — so the worker stays oblivious to them.
type RuntimeExecutor struct {
	Leaser rt.SecretLeaser
	LLM    rt.LLM
	Opts   []rt.RunOption
}

// Execute runs one turn through the runtime. It is the only call site that
// couples turnmodel to RunTurn.
func (r RuntimeExecutor) Execute(ctx context.Context, t Turn) (Result, error) {
	return rt.RunTurn(ctx, t.Agent, t.Spec, r.Leaser, r.LLM, r.Opts...)
}

// ExecutorFunc adapts a plain function to a TurnExecutor — a convenience for
// tests (and any caller) that want a fake executor without a named type.
type ExecutorFunc func(ctx context.Context, t Turn) (Result, error)

// Execute satisfies TurnExecutor.
func (f ExecutorFunc) Execute(ctx context.Context, t Turn) (Result, error) { return f(ctx, t) }
