package v1

import (
	"testing"
	"time"
)

func validTeam() AgentTeam {
	return AgentTeam{
		Name: "t",
		Spec: AgentTeamSpec{
			Lead: "coordinator",
			Members: []TeamMemberSpec{
				{Name: "researcher", AgentRef: "researcher-agent"},
				{Name: "critic", AgentRef: "critic-agent"},
			},
			Pattern: TeamPatternTeam,
			Budget:  &Budget{MaxSteps: 100, MaxTokens: 500000, MaxWallClockSeconds: 600, MaxToolCalls: 50},
		},
	}
}

func TestValidateAgentTeam_OK(t *testing.T) {
	if err := ValidateAgentTeam(validTeam()); err != nil {
		t.Fatalf("valid team rejected: %v", err)
	}
	// Pattern + budget are optional.
	bare := AgentTeam{Name: "t", Spec: AgentTeamSpec{Lead: "lead", Members: []TeamMemberSpec{{Name: "m", AgentRef: "a"}}}}
	if err := ValidateAgentTeam(bare); err != nil {
		t.Fatalf("minimal team rejected: %v", err)
	}
}

func TestValidateAgentTeam_Rejections(t *testing.T) {
	cases := map[string]func(*AgentTeam){
		"no lead":               func(tm *AgentTeam) { tm.Spec.Lead = "" },
		"cross-ns lead":         func(tm *AgentTeam) { tm.Spec.Lead = "other/lead" },
		"no members":            func(tm *AgentTeam) { tm.Spec.Members = nil },
		"member no name":        func(tm *AgentTeam) { tm.Spec.Members[0].Name = "" },
		"member no agentRef":    func(tm *AgentTeam) { tm.Spec.Members[0].AgentRef = "" },
		"cross-ns member":       func(tm *AgentTeam) { tm.Spec.Members[0].AgentRef = "other/agent" },
		"duplicate member name": func(tm *AgentTeam) { tm.Spec.Members[1].Name = tm.Spec.Members[0].Name },
		"bad pattern":           func(tm *AgentTeam) { tm.Spec.Pattern = "freeform" },
		"negative maxMembers":   func(tm *AgentTeam) { tm.Spec.MaxMembers = -1 },
		"invalid budget":        func(tm *AgentTeam) { tm.Spec.Budget = &Budget{MaxSteps: 0, MaxTokens: 1, MaxWallClockSeconds: 1} },
		"bad hook event":        func(tm *AgentTeam) { tm.Spec.Hooks = []TeamHookSpec{{Event: "Nope", Action: HookActionVeto}} },
		"bad hook action":       func(tm *AgentTeam) { tm.Spec.Hooks = []TeamHookSpec{{Event: TeamHookTaskCreated, Action: "maybe"}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tm := validTeam()
			mutate(&tm)
			if err := ValidateAgentTeam(tm); err == nil {
				t.Fatalf("%s: expected validation error, got nil", name)
			}
		})
	}
}

func TestValidateAgentTeam_Hooks(t *testing.T) {
	tm := validTeam()
	tm.Spec.Hooks = []TeamHookSpec{
		{Event: TeamHookTaskCreated, Action: HookActionVeto, Reason: "needs sign-off"},
		{Event: TeamHookTeammateIdle, Action: HookActionRequeue},
		{Event: TeamHookTaskCompleted, Action: HookActionAllow},
	}
	if err := ValidateAgentTeam(tm); err != nil {
		t.Fatalf("valid hooks rejected: %v", err)
	}
}

func TestValidateAgentTeam_Convergence(t *testing.T) {
	// generator-verifier REQUIRES a convergence spec (no intrinsic stop).
	genver := validTeam()
	genver.Spec.Pattern = TeamPatternGeneratorVerifier
	if err := ValidateAgentTeam(genver); err == nil {
		t.Fatalf("generator-verifier without convergence must be rejected")
	}
	// ...and is valid with a well-formed one.
	genver.Spec.Convergence = &ConvergenceSpec{MaxIterations: 5, Criteria: "answer cites sources"}
	if err := ValidateAgentTeam(genver); err != nil {
		t.Fatalf("generator-verifier with convergence rejected: %v", err)
	}
	// orchestrator does NOT require convergence.
	orch := validTeam()
	orch.Spec.Pattern = TeamPatternOrchestrator
	orch.Spec.Convergence = nil
	if err := ValidateAgentTeam(orch); err != nil {
		t.Fatalf("orchestrator without convergence must be allowed: %v", err)
	}
	// A present-but-malformed convergence is rejected on any pattern.
	for _, c := range []*ConvergenceSpec{
		{MaxIterations: 0, Criteria: "c"},
		{MaxIterations: 3, Criteria: ""},
		{MaxIterations: 3, Criteria: "c", TimeBudgetSeconds: -1},
	} {
		tm := validTeam()
		tm.Spec.Convergence = c
		if err := ValidateAgentTeam(tm); err == nil {
			t.Fatalf("malformed convergence %+v must be rejected", c)
		}
	}
}

func TestValidateAgentTeam_SharedWorkspace(t *testing.T) {
	ok := validTeam()
	ok.Spec.SharedWorkspace = &SharedWorkspaceSpec{SizeGiB: 2, ConflictMode: ConflictBranchMerge}
	if err := ValidateAgentTeam(ok); err != nil {
		t.Fatalf("valid shared workspace rejected: %v", err)
	}
	if got := (SharedWorkspaceSpec{}).EffectiveConflictMode(); got != ConflictSharedRW {
		t.Fatalf("default conflict mode = shared-rw, got %q", got)
	}
	for name, w := range map[string]*SharedWorkspaceSpec{
		"zero size":    {SizeGiB: 0},
		"bad conflict": {SizeGiB: 1, ConflictMode: "yolo"},
	} {
		tm := validTeam()
		tm.Spec.SharedWorkspace = w
		if err := ValidateAgentTeam(tm); err == nil {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}

func TestRollUpTeamUsage_FieldWise(t *testing.T) {
	members := []Usage{
		{Steps: 3, Tokens: 100, ToolCalls: 2, WallClockUsed: 5 * time.Second, CostUSDMilli: 7},
		{Steps: 4, Tokens: 200, ToolCalls: 1, WallClockUsed: 9 * time.Second, CostUSDMilli: 3},
	}
	got := RollUpTeamUsage(members)
	if got.Steps != 7 || got.Tokens != 300 || got.ToolCalls != 3 || got.CostUSDMilli != 10 {
		t.Fatalf("field-wise sum wrong: %+v", got)
	}
	// WallClock is NOT summed (members run concurrently — team elapsed is its own).
	if got.WallClockUsed != 0 {
		t.Fatalf("wallClock must not be summed, got %v", got.WallClockUsed)
	}
	// Empty input → zero usage.
	if (RollUpTeamUsage(nil) != Usage{}) {
		t.Fatalf("empty roll-up must be zero")
	}
}

func TestAgentTeamSpec_Effectives(t *testing.T) {
	s := AgentTeamSpec{Members: []TeamMemberSpec{{Name: "a"}, {Name: "b"}, {Name: "c"}}}
	if got := s.EffectiveMaxMembers(); got != 3 {
		t.Fatalf("default maxMembers = len(members): want 3, got %d", got)
	}
	s.MaxMembers = 2
	if got := s.EffectiveMaxMembers(); got != 2 {
		t.Fatalf("explicit maxMembers wins: want 2, got %d", got)
	}
	if got := (AgentTeamSpec{}).EffectivePattern(); got != TeamPatternOrchestrator {
		t.Fatalf("default pattern = orchestrator, got %q", got)
	}
}
