// Package coordinator holds the runtime adapters that bind the pure AgentTeam
// coordinator logic (pkg/turnmodel/team) to the live runtime: the generator seam
// (A2ADispatcher — delegate a round to a member Agent over the A2A invoker) and
// the verifier seam (NewJudgeVerifier — grade with the run's own loop LLM). A
// coordinator-pod-driven team turn (rv3.1) wires these into team.Coordinate.
//
// This package imports pkg/turnmodel/team (and agentruntime), never the reverse,
// so the pure logic stays dependency-free; the concrete invoker is reached only
// through the abstract agentruntime.ToolInvoker, so nothing here depends on how
// the operator wires the A2A invoker.
package coordinator

import (
	"context"
	"encoding/json"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/turnmodel/team"
)

// A2ADispatcher is the live team.MemberDispatcher: it delegates each
// generator-verifier round to a member Agent by spawning a child AgentRun via the
// kind=agent (A2A) invoker, then folds the child's terminal observation into a
// team.Attempt. It is the generator half of the coordinator loop (the verifier
// half is NewJudgeVerifier). Invoker is the abstract agentruntime.ToolInvoker —
// *invokers.AgentRunInvoker live, a fake in tests — so this adapter never depends
// on the concrete invoker construction.
type A2ADispatcher struct {
	// Invoker spawns + blocks on the child AgentRun (kind=agent). Required.
	Invoker agentruntime.ToolInvoker
	// Member is the member Agent's bare name (tool.spec.agent.ref.name), in the
	// coordinator's own namespace (D1 — no cross-tenant delegation). Required.
	Member string
	// MaxTokens optionally caps each member round's tokens (a budgetOverride on the
	// child run). 0 = the member Agent's own budget governs.
	MaxTokens int64
	// TimeoutSeconds optionally bounds ONE member round (the A2A per-call timeout).
	// 0 = bounded only by the coordinator's run deadline.
	TimeoutSeconds int32
}

// A2ADispatcher is a team.MemberDispatcher.
var _ team.MemberDispatcher = (*A2ADispatcher)(nil)

// memberInput is the objective the coordinator hands a member each round: the
// fixed goal, the verifier's rolling feedback (empty on round 1), and the 1-based
// round, marshalled as the child run's input.
type memberInput struct {
	Objective string `json:"objective"`
	Feedback  string `json:"feedback,omitempty"`
	Round     int    `json:"round"`
}

// Dispatch spawns one member run for round with the objective + prior feedback and
// returns its folded output as an Attempt. Usage maps the A2A observation's
// field-wise tokens + toolCalls — the only axes a child observation surfaces
// (steps and cost are not carried by the A2A fold), and tokens/toolCalls are
// exactly what the team budget gates on.
func (d *A2ADispatcher) Dispatch(ctx context.Context, round int, objective, feedback string) (team.Attempt, error) {
	tool := v1.Tool{
		Name: "delegate:" + d.Member,
		Spec: v1.ToolSpec{
			Kind: v1.ToolAgent,
			Agent: &v1.AgentTargetSpec{
				Ref:            v1.ToolRef{Name: d.Member},
				MaxTokens:      d.MaxTokens,
				TimeoutSeconds: d.TimeoutSeconds,
			},
		},
	}
	args, err := json.Marshal(memberInput{Objective: objective, Feedback: feedback, Round: round})
	if err != nil {
		return team.Attempt{}, err
	}
	obs, err := d.Invoker.Invoke(ctx, tool, args)
	if err != nil {
		return team.Attempt{}, err
	}
	return team.Attempt{
		Content: observationContent(obs.Output),
		Usage:   v1.Usage{Tokens: obs.Tokens, ToolCalls: obs.ToolCalls},
	}, nil
}

// observationContent renders a child run's folded output as the candidate text the
// verifier grades: a JSON string is unwrapped to its value (the common case — an
// agent that answered with a plain string), and any other JSON (an object/array)
// is passed through verbatim so structured answers reach the judge intact.
func observationContent(out json.RawMessage) string {
	if len(out) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(out, &s) == nil {
		return s
	}
	return string(out)
}
