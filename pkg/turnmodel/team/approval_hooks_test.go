package team

import (
	"testing"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func TestCoordinatorDecision(t *testing.T) {
	pa := pure.PendingAction{Kind: "pre-run", Token: "tok-123"}
	d := CoordinatorDecision(pa, "lead", true, "plan looks good")
	if d.Token != "tok-123" {
		t.Fatalf("must copy the pending-action token (M5 token-match): %q", d.Token)
	}
	if !d.Approve || d.Reason != "plan looks good" {
		t.Fatalf("approve/reason not carried: %+v", d)
	}
	if d.DecidedBy != "coordinator:lead" {
		t.Fatalf("decidedBy must record the coordinator for audit: %q", d.DecidedBy)
	}
	// Deny path.
	if dn := CoordinatorDecision(pa, "lead", false, "scope creep"); dn.Approve {
		t.Fatalf("deny must set Approve=false")
	}
}

func TestTeamHooksFromSpec(t *testing.T) {
	// Empty/absent spec → nil (no gates → everything allowed).
	if got := TeamHooksFromSpec(nil); got != nil {
		t.Fatalf("empty spec should convert to nil, got %v", got)
	}
	specs := []pure.TeamHookSpec{
		{Event: pure.TeamHookTaskCreated, Action: pure.HookActionVeto, Reason: "over budget"},
		{Event: pure.TeamHookTeammateIdle, Action: pure.HookActionRequeue},
	}
	hooks := TeamHooksFromSpec(specs)
	if len(hooks) != 2 {
		t.Fatalf("want 2 hooks, got %d", len(hooks))
	}
	// The CRD enum strings map 1:1 onto this package's constants.
	if hooks[0].Event != HookTaskCreated || hooks[0].Action != HookVeto || hooks[0].Reason != "over budget" {
		t.Fatalf("hook[0] not mapped: %+v", hooks[0])
	}
	// The converted hooks drive EvaluateHooks exactly as hand-built ones would.
	if a, r := EvaluateHooks(HookTaskCreated, hooks); a != HookVeto || r != "over budget" {
		t.Fatalf("converted task-created hook should veto: %q/%q", a, r)
	}
	if a, _ := EvaluateHooks(HookTeammateIdle, hooks); a != HookRequeue {
		t.Fatalf("converted idle hook should requeue: %q", a)
	}
}

func TestEvaluateHooks(t *testing.T) {
	hooks := []TeamHook{
		{Event: HookTaskCreated, Action: HookVeto, Reason: "over budget"},
		{Event: HookTeammateIdle, Action: HookRequeue},
		{Event: HookTaskCompleted, Action: HookAllow}, // explicit allow is a no-op
	}
	if a, r := EvaluateHooks(HookTaskCreated, hooks); a != HookVeto || r != "over budget" {
		t.Fatalf("task-created should veto: %q/%q", a, r)
	}
	if a, _ := EvaluateHooks(HookTeammateIdle, hooks); a != HookRequeue {
		t.Fatalf("idle should requeue: %q", a)
	}
	if a, _ := EvaluateHooks(HookTaskCompleted, hooks); a != HookAllow {
		t.Fatalf("explicit allow → allow: %q", a)
	}
	// No matching hook → allow (fail-open only in the sense that an unconfigured
	// event is permitted; a configured veto/requeue always wins).
	if a, _ := EvaluateHooks(HookTaskCreated, nil); a != HookAllow {
		t.Fatalf("no hooks → allow: %q", a)
	}
}
