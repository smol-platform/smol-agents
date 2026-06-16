package team

import "context"

// MemberDispatcher delegates one round of work to a team member and returns its
// result as an Attempt. The live implementation wraps the A2A AgentRun invoker
// (spawn a child run for the member's Agent, block to terminal, fold its output +
// usage); tests bind a fake. Keeping this an interface keeps pkg/turnmodel/team
// free of any kubernetes / runtime dependency — pure logic, with I/O at the edge.
type MemberDispatcher interface {
	// Dispatch runs the member for round (1-based) with the fixed objective and the
	// prior verifier's feedback (empty on round 1), returning the member's attempt.
	Dispatch(ctx context.Context, round int, objective, feedback string) (Attempt, error)
}

// GeneratorOverDispatch adapts a MemberDispatcher into the loop's GeneratorFunc:
// each generator round delegates to the member with the fixed objective and the
// verifier's rolling feedback. This is the A2A generator seam — the missing half
// the convergence loop needed for a live coordinator (the verifier half is
// JudgeVerifier).
func GeneratorOverDispatch(d MemberDispatcher, objective string) GeneratorFunc {
	return func(ctx context.Context, round int, feedback string) (Attempt, error) {
		return d.Dispatch(ctx, round, objective, feedback)
	}
}
