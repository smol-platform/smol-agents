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

func runFor(name, agent string, pri int32, sec int64) amv1.AgentRun {
	r := runWith(name, pri, sec)
	r.Spec.AgentRef = agent
	return r
}

// rv2.3: within a priority tier, ordering is PER-AGENT ROUND-ROBIN, so a quiet
// Agent's single run is not starved behind a noisy Agent's backlog. Agent A has
// a backlog (t0,t1,t2); Agent B has one run at t1.5. B's run is its Agent's 1st
// (local index 0), so it only waits behind other Agents' 1st runs — here A's t0
// — NOT behind A's t1/t2. Strict FIFO would put B behind both A@t0 and A@t1.
func TestRankAhead_PerAgentFairness(t *testing.T) {
	r := &AgentRunReconciler{MaxPriority: 1000}
	queued := []amv1.AgentRun{
		runFor("a0", "a", 0, 0),
		runFor("a1", "a", 0, 1),
		runFor("a2", "a", 0, 2),
		runFor("b0", "b", 0, 1), // B's only run, arrives mid-backlog
	}

	b := runFor("b0", "b", 0, 1)
	if got := r.rankAhead(queued, &b); got != 1 {
		t.Errorf("rankAhead(B's 1st run) = %d, want 1 (only A's 1st ahead; FIFO would give 2)", got)
	}
	// A's 3rd run waits behind everyone's earlier-slot runs, incl. B's 1st.
	a2 := runFor("a2", "a", 0, 2)
	if got := r.rankAhead(queued, &a2); got != 3 {
		t.Errorf("rankAhead(A's 3rd run) = %d, want 3 (A@t0, A@t1, B@t1.5)", got)
	}
	// Fairness invariant: the quiet Agent's run is admitted before the noisy
	// Agent's later runs.
	if r.rankAhead(queued, &b) >= r.rankAhead(queued, &a2) {
		t.Error("quiet Agent B's run should rank ahead of noisy Agent A's 3rd run")
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
