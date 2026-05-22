package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// FakeLLM replays a scripted sequence of decisions. Useful in tests +
// property tests where we need bit-exact determinism.
type FakeLLM struct {
	mu        sync.Mutex
	Script    []rt.LLMDecision
	cursor    int
	TokensIn  int64 // tokens reported per call (constant)
	TokensOut int64
}

func (f *FakeLLM) Chat(_ context.Context, _ ChatRequest) (rt.LLMDecision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cursor >= len(f.Script) {
		// Default to "I'm done" so we never hang in tests.
		return rt.LLMDecision{
			FinalAnswer: &rt.FinalAnswer{Output: json.RawMessage(`{"answer":"done"}`)},
			TokensIn:    f.TokensIn,
			TokensOut:   f.TokensOut,
		}, nil
	}
	d := f.Script[f.cursor]
	f.cursor++
	if d.TokensIn == 0 {
		d.TokensIn = f.TokensIn
	}
	if d.TokensOut == 0 {
		d.TokensOut = f.TokensOut
	}
	return d, nil
}

// InProcessInvoker is a tool invoker backed by a Go map of handlers.
// Used for tests; it never touches network.
type InProcessInvoker struct {
	mu       sync.Mutex
	Handlers map[string]func(args json.RawMessage) (json.RawMessage, error)
	Latency  time.Duration
}

func (i *InProcessInvoker) Invoke(_ context.Context, tool v1.Tool, args json.RawMessage) (rt.Observation, error) {
	i.mu.Lock()
	h, ok := i.Handlers[tool.Name]
	lat := i.Latency
	i.mu.Unlock()
	if !ok {
		return rt.Observation{}, errors.New("InProcessInvoker: no handler for " + tool.Name)
	}
	out, err := h(args)
	if err != nil {
		return rt.Observation{}, err
	}
	return rt.Observation{Output: out, DurationMs: lat.Milliseconds()}, nil
}

// FakeClock is a deterministic Clock used in tests.
type FakeClock struct {
	mu sync.Mutex
	T  time.Time
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.T
}

func (c *FakeClock) Since(t time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.T.Sub(t)
}

// Advance moves the clock forward by d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.T = c.T.Add(d)
}
