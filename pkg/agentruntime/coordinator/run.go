package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/turnmodel/team"
)

// ErrNoMembers is returned when a coordinator team declares no member to drive
// the generator-verifier loop. ValidateAgentTeam forbids a memberless team, so
// this is a defensive runtime check, not a normal path.
var ErrNoMembers = errors.New("coordinator: team has no members to drive the generator-verifier loop")

// BuildCoordinatorConfig assembles the pure team.CoordinatorConfig from an
// AgentTeam's spec: the convergence bound, the team-budget backstop, the
// lifecycle hooks (CRD → domain via TeamHooksFromSpec), and the lead's identity.
// A nil convergence leaves Spec zero (RunGeneratorVerifier then rejects it); a
// nil budget leaves Guard nil (no team-budget backstop — only the pod deadline).
func BuildCoordinatorConfig(at pure.AgentTeam) team.CoordinatorConfig {
	cfg := team.CoordinatorConfig{Coordinator: at.Spec.Lead}
	if at.Spec.Convergence != nil {
		cfg.Spec = *at.Spec.Convergence
	}
	if at.Spec.Budget != nil {
		cfg.Guard = &team.BudgetGuard{Budget: *at.Spec.Budget}
	}
	cfg.Hooks = team.TeamHooksFromSpec(at.Spec.Hooks)
	return cfg
}

// GeneratorMember returns the member that drives the generator-verifier loop. The
// pattern has a single generator, so we use the first declared member (a
// generator-verifier team declares its generator first); the verifier is the
// LLM-as-judge (NewJudgeVerifier), not a member. Returns false when the team
// declares no members.
func GeneratorMember(at pure.AgentTeam) (pure.TeamMemberSpec, bool) {
	if len(at.Spec.Members) == 0 {
		return pure.TeamMemberSpec{}, false
	}
	return at.Spec.Members[0], true
}

// DecodeAgentTeamSpec decodes a JSON AgentTeam spec (read in-pod as unstructured,
// then marshalled) into the pure AgentTeamSpec. Kept here so the cmd/agent entry
// stays thin and the decode is unit-testable.
func DecodeAgentTeamSpec(specJSON []byte) (pure.AgentTeamSpec, error) {
	var spec pure.AgentTeamSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return pure.AgentTeamSpec{}, fmt.Errorf("coordinator: decode AgentTeam spec: %w", err)
	}
	return spec, nil
}

// Run executes one coordinator-pod-driven generator-verifier turn: assemble the
// config from the team, dispatch generation to the generator member over the A2A
// invoker, verify with the judge, and return the converged result. It is pure
// given its injected seams (invoker + verify), so the orchestration is
// unit-testable without a real cluster or model — the cmd/agent entry supplies the
// live A2A invoker, the judge built from the loop LLM, and the team read in-pod.
func Run(ctx context.Context, at pure.AgentTeam, objective string, invoker agentruntime.ToolInvoker, verify team.VerifierFunc) (team.CoordinatorResult, error) {
	gm, ok := GeneratorMember(at)
	if !ok {
		return team.CoordinatorResult{}, ErrNoMembers
	}
	cfg := BuildCoordinatorConfig(at)
	d := &A2ADispatcher{Invoker: invoker, Member: gm.AgentRef}
	gen := team.GeneratorOverDispatch(d, objective)
	return team.Coordinate(ctx, cfg, gen, verify)
}
