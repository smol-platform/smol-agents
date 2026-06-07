// Package team is the coordination logic for an AgentTeam (multi-agent
// orchestration, P3): the generator-verifier convergence loop, the team-budget
// hard backstop, and the orchestrator width limiter. Per the resolved decisions
// the coordinator is a loop-mode agent; this package is the deterministic control
// logic it drives, so termination and budget are enforced by the framework, not
// left to a prompt. The generator/verifier seams are functions, so the loop is
// testable without real agents (the live coordinator wires them to A2A calls).
package team

import (
	"context"
	"errors"
	"time"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// Attempt is one candidate solution a generator produced.
type Attempt struct {
	Content string
	Usage   pure.Usage
}

// Verdict is a verifier's judgment of an Attempt against the criteria.
type Verdict struct {
	Accepted bool
	// Score ranks attempts (higher is better) so the best is returned on
	// non-convergence.
	Score int
	// Feedback is passed to the next generator round.
	Feedback string
	Usage    pure.Usage
}

// GeneratorFunc produces an attempt for round r (1-based), given the prior
// verdict's feedback (empty on the first round).
type GeneratorFunc func(ctx context.Context, round int, feedback string) (Attempt, error)

// VerifierFunc judges an attempt against the criteria.
type VerifierFunc func(ctx context.Context, a Attempt, criteria string) (Verdict, error)

// ConvergenceResult is the outcome of RunGeneratorVerifier.
type ConvergenceResult struct {
	Best     Attempt
	Verdict  Verdict
	Accepted bool
	Rounds   int
	// Usage is the field-wise total across all rounds (generator + verifier).
	Usage pure.Usage
	// StopReason is accepted | max-iterations | time-budget | team-budget.
	StopReason string
}

// ErrInvalidConvergence is returned for a spec the validator would reject.
var ErrInvalidConvergence = errors.New("team: invalid convergence spec")

// RunGeneratorVerifier loops generator→verifier until the verifier accepts, the
// iteration cap is hit, the time budget elapses, or the team-budget guard trips.
// It always returns the best attempt seen (highest verifier score) — never an
// open-ended loop (the blog's #1 failure mode). Usage rolls up FIELD-WISE across
// rounds; a nil guard means no team-budget backstop here (the pod deadline still
// applies live).
func RunGeneratorVerifier(ctx context.Context, spec pure.ConvergenceSpec, gen GeneratorFunc, verify VerifierFunc, guard *BudgetGuard) (ConvergenceResult, error) {
	if spec.MaxIterations < 1 || spec.Criteria == "" {
		return ConvergenceResult{}, ErrInvalidConvergence
	}
	if spec.TimeBudgetSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(spec.TimeBudgetSeconds)*time.Second)
		defer cancel()
	}

	var res ConvergenceResult
	haveBest := false
	feedback := ""
	for round := 1; round <= int(spec.MaxIterations); round++ {
		if ctx.Err() != nil {
			res.StopReason = "time-budget"
			return res, nil
		}
		if guard != nil {
			if err := guard.AllowsMore(res.Usage); err != nil {
				res.StopReason = "team-budget"
				return res, nil
			}
		}

		a, err := gen(ctx, round, feedback)
		if err != nil {
			return res, err
		}
		res.Usage = addUsage(res.Usage, a.Usage)
		res.Rounds = round

		v, err := verify(ctx, a, spec.Criteria)
		if err != nil {
			return res, err
		}
		res.Usage = addUsage(res.Usage, v.Usage)

		if !haveBest || v.Score > res.Verdict.Score {
			haveBest = true
			res.Best = a
			res.Verdict = v
		}
		if v.Accepted {
			res.Accepted = true
			res.StopReason = "accepted"
			return res, nil
		}
		feedback = v.Feedback
	}
	res.StopReason = "max-iterations"
	return res, nil
}

// addUsage folds b into a FIELD-WISE (never Usage.Add); WallClock is not summed
// (matches the team roll-up — concurrent work, elapsed is the loop's own).
func addUsage(a, b pure.Usage) pure.Usage {
	a.Steps += b.Steps
	a.Tokens += b.Tokens
	a.ToolCalls += b.ToolCalls
	a.CostUSDMilli += b.CostUSDMilli
	return a
}
