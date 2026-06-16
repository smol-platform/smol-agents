package team

import (
	"context"
	"testing"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// acceptOnRound returns a VerifierFunc that accepts at round n (tracked by call
// count) and otherwise rejects with rolling feedback so the generator can see it.
func acceptOnRound(n int) (VerifierFunc, *int) {
	calls := 0
	vf := func(ctx context.Context, a Attempt, criteria string) (Verdict, error) {
		calls++
		if calls >= n {
			return Verdict{Accepted: true, Score: 100}, nil
		}
		return Verdict{Accepted: false, Score: calls, Feedback: "needs work"}, nil
	}
	return vf, &calls
}

func constGen(content string, u pure.Usage) (GeneratorFunc, *int) {
	calls := 0
	gf := func(ctx context.Context, round int, feedback string) (Attempt, error) {
		calls++
		return Attempt{Content: content, Usage: u}, nil
	}
	return gf, &calls
}

func baseSpec() pure.ConvergenceSpec {
	return pure.ConvergenceSpec{MaxIterations: 5, Criteria: "be correct"}
}

// The happy path: the verifier accepts on the first round, no hooks → admitted,
// accepted, one round, reason "accepted".
func TestCoordinate_AcceptsFirstRound(t *testing.T) {
	gen, genCalls := constGen("answer", pure.Usage{Tokens: 10})
	verify, _ := acceptOnRound(1)

	res, err := Coordinate(context.Background(), CoordinatorConfig{Spec: baseSpec()}, gen, verify)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Admitted || !res.Accepted {
		t.Fatalf("want admitted+accepted, got admitted=%v accepted=%v", res.Admitted, res.Accepted)
	}
	if res.StopReason != "accepted" {
		t.Errorf("StopReason = %q, want accepted", res.StopReason)
	}
	if res.Convergence.Rounds != 1 || *genCalls != 1 {
		t.Errorf("rounds=%d genCalls=%d, want 1/1", res.Convergence.Rounds, *genCalls)
	}
	if res.Convergence.Best.Content != "answer" {
		t.Errorf("best content = %q, want answer", res.Convergence.Best.Content)
	}
}

// A TaskCreated veto refuses the turn fail-closed BEFORE any member work runs:
// the generator must never be called.
func TestCoordinate_TaskCreatedVetoBlocks(t *testing.T) {
	gen, genCalls := constGen("answer", pure.Usage{})
	verify, verifyCalls := acceptOnRound(1)
	hooks := []TeamHook{{Event: HookTaskCreated, Action: HookVeto, Reason: "policy: needs human sign-off"}}

	res, err := Coordinate(context.Background(), CoordinatorConfig{Spec: baseSpec(), Hooks: hooks}, gen, verify)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Admitted {
		t.Error("TaskCreated veto must leave Admitted=false")
	}
	if res.Accepted {
		t.Error("a vetoed turn is never accepted")
	}
	if res.StopReason != stopVetoedCreate {
		t.Errorf("StopReason = %q, want %q", res.StopReason, stopVetoedCreate)
	}
	if res.HookReason != "policy: needs human sign-off" {
		t.Errorf("HookReason = %q, want the veto reason", res.HookReason)
	}
	if *genCalls != 0 || *verifyCalls != 0 {
		t.Errorf("vetoed turn ran work: genCalls=%d verifyCalls=%d, want 0/0", *genCalls, *verifyCalls)
	}
}

// A TaskCompleted veto downgrades an otherwise-accepted result (a quality gate):
// the loop accepted, but the final verdict is rejected with the gate's reason.
func TestCoordinate_TaskCompletedVetoDowngrades(t *testing.T) {
	gen, _ := constGen("answer", pure.Usage{})
	verify, _ := acceptOnRound(1)
	hooks := []TeamHook{{Event: HookTaskCompleted, Action: HookVeto, Reason: "fails the lint gate"}}

	res, err := Coordinate(context.Background(), CoordinatorConfig{Spec: baseSpec(), Hooks: hooks}, gen, verify)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Admitted {
		t.Error("TaskCompleted gate must not block admission")
	}
	if res.Accepted {
		t.Error("a TaskCompleted veto must downgrade Accepted to false")
	}
	if !res.Convergence.Accepted {
		t.Error("the verifier DID accept; Convergence.Accepted should stay true (only the gate rejected)")
	}
	if res.StopReason != stopVetoedComplete {
		t.Errorf("StopReason = %q, want %q", res.StopReason, stopVetoedComplete)
	}
	if res.HookReason != "fails the lint gate" {
		t.Errorf("HookReason = %q, want the gate reason", res.HookReason)
	}
}

// The completion gate is only consulted on an accepted loop: when the loop hits
// max-iterations (never accepted), a TaskCompleted veto is irrelevant and the
// reason stays max-iterations.
func TestCoordinate_CompletionGateIgnoredWhenNotAccepted(t *testing.T) {
	gen, _ := constGen("answer", pure.Usage{})
	verify, _ := acceptOnRound(99) // never accepts within MaxIterations
	hooks := []TeamHook{{Event: HookTaskCompleted, Action: HookVeto, Reason: "irrelevant"}}

	res, err := Coordinate(context.Background(), CoordinatorConfig{Spec: baseSpec(), Hooks: hooks}, gen, verify)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Accepted {
		t.Error("loop never accepted; result must not be accepted")
	}
	if res.StopReason != "max-iterations" {
		t.Errorf("StopReason = %q, want max-iterations (completion gate not consulted)", res.StopReason)
	}
	if res.Convergence.Rounds != 5 {
		t.Errorf("rounds = %d, want 5 (the iteration cap)", res.Convergence.Rounds)
	}
}

// The team-budget guard is the hard backstop beneath convergence: with a tiny
// token ceiling and a never-accepting verifier, the loop stops on team-budget,
// not the iteration cap, and still returns the best attempt.
func TestCoordinate_TeamBudgetBackstop(t *testing.T) {
	gen, _ := constGen("answer", pure.Usage{Tokens: 100})
	verify, _ := acceptOnRound(99)
	guard := &BudgetGuard{Budget: pure.Budget{MaxSteps: 1 << 30, MaxTokens: 50, MaxWallClockSeconds: 1 << 30}}

	res, err := Coordinate(context.Background(), CoordinatorConfig{Spec: baseSpec(), Guard: guard}, gen, verify)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Round 1 runs (cumulative usage 0 < 50); round 2's pre-check sees 100 ≥ 50.
	if res.StopReason != "team-budget" {
		t.Errorf("StopReason = %q, want team-budget", res.StopReason)
	}
	if res.Convergence.Rounds != 1 {
		t.Errorf("rounds = %d, want 1 (budget trips before round 2)", res.Convergence.Rounds)
	}
	if res.Convergence.Best.Content != "answer" {
		t.Error("budget stop must still return the best attempt seen")
	}
}

// fakeDispatcher records each Dispatch call so GeneratorOverDispatch can be
// asserted to thread round + objective + rolling feedback correctly.
type fakeDispatcher struct {
	calls []struct {
		round           int
		objective, feed string
	}
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, round int, objective, feedback string) (Attempt, error) {
	f.calls = append(f.calls, struct {
		round           int
		objective, feed string
	}{round, objective, feedback})
	return Attempt{Content: "member-output", Usage: pure.Usage{Tokens: 5}}, nil
}

// GeneratorOverDispatch threads the fixed objective + the verifier's rolling
// feedback into the member dispatch each round.
func TestGeneratorOverDispatch_ThreadsObjectiveAndFeedback(t *testing.T) {
	fd := &fakeDispatcher{}
	gen := GeneratorOverDispatch(fd, "ship the fix")
	verify, _ := acceptOnRound(2) // round 1 rejects with "needs work", round 2 accepts

	res, err := Coordinate(context.Background(), CoordinatorConfig{Spec: baseSpec()}, gen, verify)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Accepted || res.Convergence.Rounds != 2 {
		t.Fatalf("want accepted in 2 rounds, got accepted=%v rounds=%d", res.Accepted, res.Convergence.Rounds)
	}
	if len(fd.calls) != 2 {
		t.Fatalf("dispatcher calls = %d, want 2", len(fd.calls))
	}
	if fd.calls[0].round != 1 || fd.calls[0].objective != "ship the fix" || fd.calls[0].feed != "" {
		t.Errorf("round 1 call = %+v, want round 1 / objective / empty feedback", fd.calls[0])
	}
	if fd.calls[1].round != 2 || fd.calls[1].feed != "needs work" {
		t.Errorf("round 2 call = %+v, want round 2 with rolling feedback", fd.calls[1])
	}
	// Usage rolls up field-wise across both rounds (member tokens + verifier tokens=0).
	if res.Convergence.Usage.Tokens != 10 {
		t.Errorf("usage tokens = %d, want 10 (2 rounds * 5)", res.Convergence.Usage.Tokens)
	}
}
