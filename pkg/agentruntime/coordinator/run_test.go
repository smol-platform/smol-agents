package coordinator

import (
	"context"
	"encoding/json"
	"testing"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/turnmodel/team"
)

func genverTeam() pure.AgentTeam {
	return pure.AgentTeam{
		Name: "squad",
		Spec: pure.AgentTeamSpec{
			Lead:        "lead",
			Members:     []pure.TeamMemberSpec{{Name: "gen", AgentRef: "generator-agent"}},
			Pattern:     pure.TeamPatternGeneratorVerifier,
			Convergence: &pure.ConvergenceSpec{MaxIterations: 4, Criteria: "cites sources"},
			Budget:      &pure.Budget{MaxSteps: 100, MaxTokens: 50000, MaxWallClockSeconds: 600, MaxToolCalls: 50},
			Hooks:       []pure.TeamHookSpec{{Event: pure.TeamHookTaskCreated, Action: pure.HookActionVeto, Reason: "frozen"}},
		},
	}
}

// BuildCoordinatorConfig maps every spec axis: convergence, team-budget guard,
// hooks (CRD→domain), and the lead identity.
func TestBuildCoordinatorConfig(t *testing.T) {
	cfg := BuildCoordinatorConfig(genverTeam())
	if cfg.Coordinator != "lead" {
		t.Errorf("coordinator = %q, want lead", cfg.Coordinator)
	}
	if cfg.Spec.MaxIterations != 4 || cfg.Spec.Criteria != "cites sources" {
		t.Errorf("convergence not carried: %+v", cfg.Spec)
	}
	if cfg.Guard == nil || cfg.Guard.Budget.MaxTokens != 50000 {
		t.Errorf("budget guard not built: %+v", cfg.Guard)
	}
	if len(cfg.Hooks) != 1 || cfg.Hooks[0].Event != team.HookTaskCreated || cfg.Hooks[0].Action != team.HookVeto {
		t.Errorf("hooks not converted: %+v", cfg.Hooks)
	}
}

// nil convergence/budget/hooks leave those controls unset (no panic, no guard).
func TestBuildCoordinatorConfig_Sparse(t *testing.T) {
	at := pure.AgentTeam{Name: "t", Spec: pure.AgentTeamSpec{Lead: "lead", Members: []pure.TeamMemberSpec{{Name: "m", AgentRef: "a"}}}}
	cfg := BuildCoordinatorConfig(at)
	if cfg.Guard != nil {
		t.Error("nil budget must leave Guard nil (no team-budget backstop)")
	}
	if cfg.Hooks != nil {
		t.Error("no hooks → nil")
	}
	if cfg.Spec.MaxIterations != 0 {
		t.Error("nil convergence → zero spec")
	}
}

func TestGeneratorMember(t *testing.T) {
	gm, ok := GeneratorMember(genverTeam())
	if !ok || gm.AgentRef != "generator-agent" {
		t.Errorf("generator member = %+v ok=%v, want generator-agent", gm, ok)
	}
	none := pure.AgentTeam{Spec: pure.AgentTeamSpec{Lead: "l"}}
	if _, ok := GeneratorMember(none); ok {
		t.Error("memberless team must report no generator")
	}
}

func TestDecodeAgentTeamSpec(t *testing.T) {
	src := genverTeam().Spec
	b, _ := json.Marshal(src)
	got, err := DecodeAgentTeamSpec(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Lead != "lead" || len(got.Members) != 1 || got.Pattern != pure.TeamPatternGeneratorVerifier {
		t.Errorf("decoded spec wrong: %+v", got)
	}
	if got.Convergence == nil || got.Convergence.MaxIterations != 4 {
		t.Errorf("convergence not decoded: %+v", got.Convergence)
	}
	if _, err := DecodeAgentTeamSpec([]byte("{not json")); err == nil {
		t.Error("invalid JSON must error")
	}
}

// Run drives the loop end-to-end with injected seams: a fake invoker generates,
// a stub verifier accepts on round 2 — proving the generator member is dispatched
// and the converged result returns.
func TestRun_DrivesGeneratorVerifier(t *testing.T) {
	at := genverTeam()
	at.Spec.Hooks = nil // don't veto admission in this happy-path test

	fi := &fakeInvoker{obs: rt.Observation{Output: json.RawMessage(`"candidate"`), Tokens: 6}}
	calls := 0
	verify := func(_ context.Context, a team.Attempt, criteria string) (team.Verdict, error) {
		calls++
		if calls >= 2 {
			return team.Verdict{Accepted: true, Score: 100}, nil
		}
		return team.Verdict{Accepted: false, Score: 1, Feedback: "more detail"}, nil
	}

	res, err := Run(context.Background(), at, "summarize the incident", fi, verify)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Accepted || res.Convergence.Rounds != 2 {
		t.Fatalf("want accepted in 2 rounds, got accepted=%v rounds=%d", res.Accepted, res.Convergence.Rounds)
	}
	if len(fi.calls) != 2 || fi.calls[0].tool.Spec.Agent.Ref.Name != "generator-agent" {
		t.Fatalf("generator member not dispatched: %d calls, first tool %+v", len(fi.calls), fi.calls[0].tool.Spec.Agent)
	}
}

// Run honors the team's admission hooks: a TaskCreated veto refuses the turn
// before any member work (the fake invoker is never called).
func TestRun_AdmissionVetoBlocksWork(t *testing.T) {
	fi := &fakeInvoker{obs: rt.Observation{Output: json.RawMessage(`"x"`)}}
	verify := func(_ context.Context, _ team.Attempt, _ string) (team.Verdict, error) {
		return team.Verdict{Accepted: true}, nil
	}
	res, err := Run(context.Background(), genverTeam(), "obj", fi, verify) // genverTeam has a TaskCreated veto
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Admitted {
		t.Error("admission veto must leave Admitted=false")
	}
	if len(fi.calls) != 0 {
		t.Errorf("vetoed turn ran %d member dispatches, want 0", len(fi.calls))
	}
}

// A memberless team is rejected defensively.
func TestRun_NoMembers(t *testing.T) {
	at := pure.AgentTeam{Name: "t", Spec: pure.AgentTeamSpec{Lead: "l", Convergence: &pure.ConvergenceSpec{MaxIterations: 1, Criteria: "x"}}}
	verify := func(_ context.Context, _ team.Attempt, _ string) (team.Verdict, error) { return team.Verdict{}, nil }
	if _, err := Run(context.Background(), at, "obj", &fakeInvoker{}, verify); err != ErrNoMembers {
		t.Errorf("err = %v, want ErrNoMembers", err)
	}
}
