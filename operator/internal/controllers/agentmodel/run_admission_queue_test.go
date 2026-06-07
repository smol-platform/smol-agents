package agentmodel

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func runWith(name string, pri int32, sec int64) amv1.AgentRun {
	return amv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "t", CreationTimestamp: metav1.Unix(sec, 0)},
		Spec:       pure.AgentRunSpec{AgentRef: "a", Priority: pri},
	}
}

// M1.13: rankAhead orders by priority desc, then creation asc, then name asc;
// clampPriority bounds to [0, MaxPriority].
func TestRankAheadAndClamp(t *testing.T) {
	r := &AgentRunReconciler{MaxPriority: 1000}
	self := runWith("self", 5, 100)
	queued := []amv1.AgentRun{
		runWith("hi", 10, 200),   // higher priority → ahead
		runWith("lo", 1, 50),     // lower priority → behind
		runWith("older", 5, 50),  // same priority, older → ahead
		runWith("newer", 5, 200), // same priority, newer → behind
		runWith("aaa", 5, 100),   // same priority + same creation, name < "self" → ahead
	}
	if got := r.rankAhead(queued, &self); got != 3 {
		t.Errorf("rankAhead = %d, want 3 (hi, older, aaa)", got)
	}
	if r.clampPriority(99999) != 1000 {
		t.Error("clamp above max → 1000")
	}
	if r.clampPriority(-5) != 0 {
		t.Error("clamp negative → 0")
	}
	if r.clampPriority(7) != 7 {
		t.Error("in-range unchanged")
	}
}

// M1.13: with the queue enabled, a lower-priority run waits while a
// higher-priority run is queued ahead for the only free slot; with the queue
// disabled (M1.12) it is admitted (under cap). Stateless — no prior in-memory
// queue, so this is also the post-failover behavior.
func TestAdmitRunConcurrency_PriorityOrder(t *testing.T) {
	sch := runtime.NewScheme()
	_ = corev1.AddToScheme(sch)
	_ = amv1.AddToScheme(sch)

	quota := &amv1.AgentRunQuota{ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "t"}}
	quota.Spec.MaxConcurrentRuns = 1
	// A higher-priority run already queued on concurrency.
	ahead := runWith("ahead", 10, 50)
	ahead.Status.State = pure.PhasePending
	ahead.Status.Reason = "ConcurrencyLimited"
	self := runWith("self", 1, 100) // lower priority

	agent := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}

	build := func(queueOn bool) (*AgentRunReconciler, *amv1.AgentRun) {
		s := self.DeepCopy()
		c := fake.NewClientBuilder().WithScheme(sch).
			WithObjects(quota, ahead.DeepCopy(), s).
			WithStatusSubresource(&amv1.AgentRun{}).Build()
		return &AgentRunReconciler{Client: c, Scheme: sch, EnableAdmissionQueue: queueOn}, s
	}

	// Queue ON → self held (the higher-priority run is ahead for the 1 slot).
	r, s := build(true)
	handled, _, _ := r.admitRunConcurrency(context.Background(), s, agent)
	if !handled {
		t.Error("queue ON: lower-priority run must wait behind the higher-priority queued run")
	}

	// Queue OFF → self admitted (under cap; M1.12 behavior unchanged).
	r2, s2 := build(false)
	handled2, _, _ := r2.admitRunConcurrency(context.Background(), s2, agent)
	if handled2 {
		t.Error("queue OFF: run under cap must be admitted (M1.12 behavior)")
	}
}
