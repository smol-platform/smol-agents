package team

import (
	"context"
	"errors"
	"testing"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestRunGeneratorVerifier_ConvergesAndAccepts(t *testing.T) {
	spec := pure.ConvergenceSpec{MaxIterations: 5, Criteria: "answer is correct"}
	gen := func(_ context.Context, round int, _ string) (Attempt, error) {
		return Attempt{Content: "attempt", Usage: pure.Usage{Steps: 1, Tokens: 10}}, nil
	}
	round := 0
	verify := func(_ context.Context, _ Attempt, _ string) (Verdict, error) {
		round++ // accept on the 2nd attempt
		return Verdict{Accepted: round >= 2, Score: round, Usage: pure.Usage{Tokens: 5}}, nil
	}
	res, err := RunGeneratorVerifier(context.Background(), spec, gen, verify, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Accepted || res.StopReason != "accepted" || res.Rounds != 2 {
		t.Fatalf("want accepted in 2 rounds: %+v", res)
	}
	// Usage rolls up field-wise: 2 gens (Steps 1, Tokens 10 each) + 2 verifies (Tokens 5 each).
	if res.Usage.Steps != 2 || res.Usage.Tokens != 30 {
		t.Fatalf("usage roll-up wrong: %+v", res.Usage)
	}
}

func TestRunGeneratorVerifier_NonConvergenceReturnsBest(t *testing.T) {
	spec := pure.ConvergenceSpec{MaxIterations: 3, Criteria: "c"}
	gen := func(_ context.Context, round int, _ string) (Attempt, error) {
		return Attempt{Content: "v", Usage: pure.Usage{Steps: 1}}, nil
	}
	score := 0
	verify := func(_ context.Context, a Attempt, _ string) (Verdict, error) {
		score += 10
		return Verdict{Accepted: false, Score: score}, nil // never accepts; score climbs
	}
	res, _ := RunGeneratorVerifier(context.Background(), spec, gen, verify, nil)
	if res.Accepted || res.StopReason != "max-iterations" || res.Rounds != 3 {
		t.Fatalf("want max-iterations after 3 rounds: %+v", res)
	}
	if res.Verdict.Score != 30 {
		t.Fatalf("best (highest score) must be the last: %+v", res.Verdict)
	}
}

func TestRunGeneratorVerifier_TeamBudgetBackstop(t *testing.T) {
	spec := pure.ConvergenceSpec{MaxIterations: 100, Criteria: "c"}
	gen := func(_ context.Context, _ int, _ string) (Attempt, error) {
		return Attempt{Usage: pure.Usage{Steps: 1}}, nil
	}
	verify := func(_ context.Context, _ Attempt, _ string) (Verdict, error) {
		return Verdict{Accepted: false, Score: 1}, nil
	}
	guard := &BudgetGuard{Budget: pure.Budget{MaxSteps: 2, MaxTokens: 1 << 30, MaxWallClockSeconds: 1 << 20, MaxToolCalls: 1 << 20}}
	res, _ := RunGeneratorVerifier(context.Background(), spec, gen, verify, guard)
	if res.StopReason != "team-budget" {
		t.Fatalf("want team-budget stop, got %q (rounds=%d)", res.StopReason, res.Rounds)
	}
	if res.Rounds != 2 {
		t.Fatalf("budget MaxSteps=2 should stop entry to round 3: rounds=%d", res.Rounds)
	}
}

func TestRunGeneratorVerifier_InvalidSpec(t *testing.T) {
	gen := func(context.Context, int, string) (Attempt, error) { return Attempt{}, nil }
	verify := func(context.Context, Attempt, string) (Verdict, error) { return Verdict{}, nil }
	for _, spec := range []pure.ConvergenceSpec{
		{MaxIterations: 0, Criteria: "c"},
		{MaxIterations: 3, Criteria: ""},
	} {
		if _, err := RunGeneratorVerifier(context.Background(), spec, gen, verify, nil); !errors.Is(err, ErrInvalidConvergence) {
			t.Fatalf("spec %+v: want ErrInvalidConvergence, got %v", spec, err)
		}
	}
}

func TestBudgetGuard_Axes(t *testing.T) {
	g := &BudgetGuard{Budget: pure.Budget{MaxSteps: 10, MaxTokens: 100, MaxWallClockSeconds: 60, MaxToolCalls: 5}}
	if err := g.AllowsMore(pure.Usage{Steps: 1, Tokens: 1, ToolCalls: 1}); err != nil {
		t.Fatalf("within budget must pass: %v", err)
	}
	for _, u := range []pure.Usage{
		{Steps: 10}, {Tokens: 100}, {ToolCalls: 5},
	} {
		if err := g.AllowsMore(u); !errors.Is(err, ErrTeamBudgetExceeded) {
			t.Fatalf("usage %+v should exceed budget", u)
		}
	}
	// MaxToolCalls=0 (forbid) must NOT spuriously trip the guard at zero usage.
	g0 := &BudgetGuard{Budget: pure.Budget{MaxSteps: 10, MaxTokens: 100, MaxWallClockSeconds: 60, MaxToolCalls: 0}}
	if err := g0.AllowsMore(pure.Usage{Steps: 1}); err != nil {
		t.Fatalf("MaxToolCalls=0 with 0 calls must pass: %v", err)
	}
}

func TestWidthLimiter(t *testing.T) {
	w := NewWidthLimiter(2)
	if err := w.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	if err := w.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if w.InUse() != 2 {
		t.Fatalf("InUse: want 2, got %d", w.InUse())
	}
	// Third acquire blocks → a cancelled ctx returns its error (does not deadlock).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Acquire(ctx); err == nil {
		t.Fatalf("acquire over cap with cancelled ctx must error")
	}
	w.Release()
	if w.InUse() != 1 {
		t.Fatalf("after release: want 1, got %d", w.InUse())
	}
	if err := w.Acquire(context.Background()); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}

	// Unlimited limiter never blocks.
	u := NewWidthLimiter(0)
	for i := 0; i < 100; i++ {
		if err := u.Acquire(context.Background()); err != nil {
			t.Fatalf("unlimited acquire: %v", err)
		}
	}
}
