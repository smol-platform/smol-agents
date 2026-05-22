package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// Request is what the executor passes to a Harness.
type Request struct {
	// Spec is the full HarnessSpec block from the Agent CR.
	Spec v1.HarnessSpec

	// Instructions is the agent's system prompt (forwarded as system
	// message / appended to the prompt depending on the harness).
	Instructions string

	// Input is the user-provided prompt for this Run, as JSON.
	Input json.RawMessage

	// WorkingDir is set when Storage.AgentFS is configured; otherwise
	// the harness is told to run in /tmp.
	WorkingDir string

	// Env is the resolved environment (literal Env values + secrets
	// fetched from the broker).
	Env map[string]string

	// Budget caps that apply to this single invocation. The harness
	// SHOULD respect them (wall clock especially); the executor
	// enforces hard timeouts independently via ctx.
	Budget v1.Budget

	// Seed is forwarded where the harness exposes one.
	Seed int64
}

// Response is what a Harness returns. The executor folds it into
// AgentRun.Status.
type Response struct {
	// Output is the harness's final answer (raw bytes).
	Output []byte

	// TokensIn / TokensOut are best-effort. Many CLI harnesses don't
	// expose token counts; the field is then 0 and the executor
	// records 0 in the Step.
	TokensIn  int64
	TokensOut int64

	// ToolCalls is best-effort: harnesses that surface a structured
	// call log fill this in; subprocess harnesses leave it empty.
	ToolCalls []v1.ToolCallRecord

	// DurationMs measured by the executor's clock.
	DurationMs int64
}

// Harness is the abstract harness backend. Implementations live in
// this package; selection happens in Registry.For.
type Harness interface {
	// Kind returns the Kind this implementation handles.
	Kind() v1.HarnessKind

	// Run invokes the harness once and returns its Response. ctx
	// cancellation MUST terminate the run (kill subprocess, abort
	// HTTP request).
	Run(ctx context.Context, req Request) (Response, error)
}

// Registry maps Kind to implementation. Defaults are wired in
// Default(); callers can swap implementations for testing.
type Registry struct {
	impls map[v1.HarnessKind]Harness
}

// Default returns a registry with all built-in harnesses registered.
func Default() *Registry {
	r := &Registry{impls: map[v1.HarnessKind]Harness{}}
	r.Register(&ClaudeCodeHarness{})
	r.Register(&CodexHarness{})
	r.Register(&AiderHarness{})
	r.Register(&GooseHarness{})
	r.Register(&GenericCLIHarness{})
	r.Register(&PiHarness{})
	r.Register(&GenericHTTPHarness{})
	return r
}

// Register adds (or replaces) an implementation. Used by tests.
func (r *Registry) Register(h Harness) {
	if r.impls == nil {
		r.impls = map[v1.HarnessKind]Harness{}
	}
	r.impls[h.Kind()] = h
}

// For returns the Harness for kind.
func (r *Registry) For(kind v1.HarnessKind) (Harness, error) {
	h, ok := r.impls[kind]
	if !ok {
		return nil, fmt.Errorf("harness: no implementation for kind %q", kind)
	}
	return h, nil
}

// Common errors visible to callers.
var (
	ErrUnsupportedKind = errors.New("harness: unsupported kind")
	ErrCancelled       = errors.New("harness: cancelled")
	ErrTimeout         = errors.New("harness: budget wallclock exceeded")
)

// budgetTimeout returns a context with the wallclock cap from the
// request's budget applied. A zero or negative budget disables the cap.
func budgetTimeout(parent context.Context, b v1.Budget) (context.Context, context.CancelFunc) {
	if b.MaxWallClockSeconds <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(b.MaxWallClockSeconds)*time.Second)
}
