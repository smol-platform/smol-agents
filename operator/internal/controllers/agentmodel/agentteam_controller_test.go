package agentmodel

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func teamScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return sch
}

func ownedRun(name, member string, team *amv1.AgentTeam, state pure.Phase, u pure.Usage) *amv1.AgentRun {
	return &amv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: team.Namespace,
			Labels:          map[string]string{TeamMemberLabel: member},
			OwnerReferences: []metav1.OwnerReference{AgentTeamOwnerRef(team)},
		},
		Spec:   pure.AgentRunSpec{AgentRef: member},
		Status: pure.RunStatus{State: state, Usage: u},
	}
}

func TestAgentTeamReconcile_RollsUpOwnedMembersFieldWise(t *testing.T) {
	sch := teamScheme(t)
	team := &amv1.AgentTeam{
		ObjectMeta: metav1.ObjectMeta{Name: "team1", Namespace: "t", UID: "team-uid-1", Generation: 1},
		Spec: pure.AgentTeamSpec{
			Lead:    "coordinator",
			Members: []pure.TeamMemberSpec{{Name: "researcher", AgentRef: "r-agent"}, {Name: "critic", AgentRef: "c-agent"}},
			Pattern: pure.TeamPatternTeam,
		},
	}
	run1 := ownedRun("m-researcher", "researcher", team, pure.PhaseRunning,
		pure.Usage{Steps: 3, Tokens: 100, ToolCalls: 2, CostUSDMilli: 7})
	run2 := ownedRun("m-critic", "critic", team, pure.PhaseCompleted,
		pure.Usage{Steps: 4, Tokens: 200, ToolCalls: 1, CostUSDMilli: 3})
	// A run NOT owned by the team must be excluded from the roll-up.
	stray := &amv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "stray", Namespace: "t"},
		Status:     pure.RunStatus{State: pure.PhaseRunning, Usage: pure.Usage{Tokens: 9999}},
	}

	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(team, run1, run2, stray).
		WithStatusSubresource(&amv1.AgentTeam{}).Build()

	r := &AgentTeamReconciler{Client: c, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "team1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got amv1.AgentTeam
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "team1"}, &got); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if got.Status.Phase != pure.PhaseRunning {
		t.Fatalf("phase: want Running (a member is running), got %q", got.Status.Phase)
	}
	cu := got.Status.CumulativeUsage
	if cu.Steps != 7 || cu.Tokens != 300 || cu.ToolCalls != 3 || cu.CostUSDMilli != 10 {
		t.Fatalf("field-wise roll-up wrong (stray must be excluded): %+v", cu)
	}
	if got.Status.ObservedGeneration != 1 {
		t.Fatalf("observedGeneration: want 1, got %d", got.Status.ObservedGeneration)
	}
	if got.Status.LastActivity == nil {
		t.Fatalf("lastActivity must be set when usage changed from zero")
	}
	if len(got.Status.Members) != 2 {
		t.Fatalf("want 2 member statuses, got %d", len(got.Status.Members))
	}
	byName := map[string]pure.TeamMemberStatus{}
	for _, m := range got.Status.Members {
		byName[m.Name] = m
	}
	if byName["researcher"].Phase != pure.PhaseRunning || byName["researcher"].Usage.Tokens != 100 {
		t.Fatalf("researcher member status wrong: %+v", byName["researcher"])
	}
	if byName["critic"].Phase != pure.PhaseCompleted || byName["critic"].Usage.Tokens != 200 {
		t.Fatalf("critic member status wrong: %+v", byName["critic"])
	}
}

func TestAgentTeamReconcile_InvalidSpecFailsClosed(t *testing.T) {
	sch := teamScheme(t)
	// No members → ValidateAgentTeam rejects; the reconciler must fail closed.
	team := &amv1.AgentTeam{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "t", Generation: 1},
		Spec:       pure.AgentTeamSpec{Lead: "coordinator"},
	}
	c := fake.NewClientBuilder().WithScheme(sch).
		WithObjects(team).WithStatusSubresource(&amv1.AgentTeam{}).Build()

	r := &AgentTeamReconciler{Client: c, Scheme: sch}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "t", Name: "bad"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got amv1.AgentTeam
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "t", Name: "bad"}, &got); err != nil {
		t.Fatalf("get team: %v", err)
	}
	if got.Status.Phase != pure.PhaseFailed || got.Status.Reason != "InvalidSpec" {
		t.Fatalf("want Failed/InvalidSpec, got %q/%q", got.Status.Phase, got.Status.Reason)
	}
}

func TestAgentTeamOwnerRef_LiteralUID(t *testing.T) {
	team := &amv1.AgentTeam{ObjectMeta: metav1.ObjectMeta{Name: "team1", Namespace: "t", UID: "uid-xyz"}}
	ref := AgentTeamOwnerRef(team)
	if ref.UID != "uid-xyz" || ref.Kind != "AgentTeam" || ref.Controller == nil || !*ref.Controller {
		t.Fatalf("owner ref must carry the literal team UID + controller=true: %+v", ref)
	}
}
