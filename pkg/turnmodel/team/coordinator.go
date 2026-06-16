package team

import (
	"context"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// CoordinatorConfig is what a coordinator-pod-driven team turn needs to drive one
// generator-verifier convergence under the team's governance (rv3.1 S5). It
// bundles the pure controls this package already enforces — the convergence bound,
// the team-budget backstop, and the lifecycle hooks — so the runtime coordinator
// only supplies the live seams (member dispatch over A2A + the judge verifier) and
// the framework owns termination, never a prompt (the blog's #1 failure mode:
// "cycle indefinitely").
type CoordinatorConfig struct {
	// Spec bounds the loop: max iterations, the verifier's criteria, and an
	// optional time budget. Required — RunGeneratorVerifier rejects an empty spec.
	Spec pure.ConvergenceSpec
	// Guard is the team-wide budget backstop BENEATH the convergence limits; nil
	// disables it here (the pod deadline still applies live).
	Guard *BudgetGuard
	// Hooks gate the turn's lifecycle, fail-closed: a TaskCreated veto refuses the
	// turn before any member work runs (admission); a TaskCompleted veto rejects an
	// otherwise-accepted result (a post-hoc quality gate). An absent hook = allow.
	Hooks []TeamHook
	// Coordinator identifies the lead agent for audit (carried into the result).
	Coordinator string
}

// CoordinatorResult is the outcome of one coordinated team turn.
type CoordinatorResult struct {
	// Convergence is the raw loop outcome (best attempt, verdict, rounds, field-
	// wise usage). Zero value when the TaskCreated gate vetoed before the loop.
	Convergence ConvergenceResult
	// Admitted is false when the TaskCreated hook vetoed the turn (no member work
	// was spawned).
	Admitted bool
	// Accepted is the FINAL verdict after the completion gate: a TaskCompleted veto
	// downgrades an accepted loop result to not-accepted, even though
	// Convergence.Accepted stays true (what the verifier alone decided).
	Accepted bool
	// StopReason is accepted | max-iterations | time-budget | team-budget |
	// vetoed-on-create | vetoed-on-complete.
	StopReason string
	// HookReason carries the vetoing hook's reason when an admission/completion gate
	// fired (empty otherwise).
	HookReason string
	// Coordinator echoes cfg.Coordinator for audit.
	Coordinator string
}

const (
	stopVetoedCreate   = "vetoed-on-create"
	stopVetoedComplete = "vetoed-on-complete"
)

// Coordinate drives ONE generator-verifier team turn under the team's hooks and
// budget — the live-coordinator glue that ties EvaluateHooks to
// RunGeneratorVerifier (rv3.1 S5). It is pure: gen/verify are the seams the
// runtime binds to A2A member dispatch (GeneratorOverDispatch) and the judge
// (JudgeVerifier), so the orchestration is unit-testable without real agents.
//
// Lifecycle:
//  1. TaskCreated gate — a veto refuses the turn fail-closed, BEFORE any member
//     run is spawned (admission). A non-veto action (allow / requeue) proceeds:
//     requeue has no meaning before the first round, so it is treated as allow.
//  2. The generator-verifier loop runs to acceptance, the iteration cap, the time
//     budget, or the team-budget backstop (RunGeneratorVerifier owns termination
//     and always returns the best attempt seen).
//  3. TaskCompleted gate — consulted only when the loop accepted; a veto rejects
//     the result (a quality gate on top of the verifier), recording the reason.
//     The best attempt is still returned for inspection.
//
// Member re-queuing on TeammateIdle is the mailbox worker path (the kind=task /
// kind=teammate invokers, already wired), not this convergence turn — so this
// function deliberately consults only the two task-lifecycle gates.
func Coordinate(ctx context.Context, cfg CoordinatorConfig, gen GeneratorFunc, verify VerifierFunc) (CoordinatorResult, error) {
	// 1. Admission gate: a TaskCreated veto blocks the turn before any work runs.
	if action, reason := EvaluateHooks(HookTaskCreated, cfg.Hooks); action == HookVeto {
		return CoordinatorResult{
			Admitted:    false,
			Accepted:    false,
			StopReason:  stopVetoedCreate,
			HookReason:  reason,
			Coordinator: cfg.Coordinator,
		}, nil
	}

	cr, err := RunGeneratorVerifier(ctx, cfg.Spec, gen, verify, cfg.Guard)
	if err != nil {
		return CoordinatorResult{Admitted: true, Coordinator: cfg.Coordinator}, err
	}

	res := CoordinatorResult{
		Convergence: cr,
		Admitted:    true,
		Accepted:    cr.Accepted,
		StopReason:  cr.StopReason,
		Coordinator: cfg.Coordinator,
	}

	// 3. Completion gate: a TaskCompleted veto downgrades an accepted result.
	if cr.Accepted {
		if action, reason := EvaluateHooks(HookTaskCompleted, cfg.Hooks); action == HookVeto {
			res.Accepted = false
			res.StopReason = stopVetoedComplete
			res.HookReason = reason
		}
	}
	return res, nil
}
