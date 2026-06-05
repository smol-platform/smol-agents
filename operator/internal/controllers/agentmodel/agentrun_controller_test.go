package agentmodel

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	"github.com/smol-platform/smol-agents/operator/internal/builders"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// runPodWithTerminationMessage builds a Pod whose `harness` container has
// already terminated with the given RunResult serialised into its termination
// message — the same shape `agent run` writes for the controller to fold.
func runPodWithTerminationMessage(rr agentruntime.RunResult) *corev1.Pod {
	msg, _ := json.Marshal(rr)
	return &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "harness",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 0,
						Reason:   "Completed",
						Message:  string(msg),
					},
				},
			}},
		},
	}
}

func sampleAgent() *amv1.Agent {
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "tenant-a"}}
	a.Spec.Mode = pure.ModeLoop
	a.Spec.Model = pure.ModelRef{ProviderRef: "openai", Name: "gpt-4"}
	a.Spec.Instructions = "be helpful"
	a.Spec.Budget = pure.Budget{MaxSteps: 5, MaxTokens: 1000, MaxWallClockSeconds: 60, MaxToolCalls: 3}
	return a
}

func sampleRun() *amv1.AgentRun {
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "run-001", Namespace: "tenant-a"}}
	r.Spec.AgentRef = "alice"
	r.Spec.Input = []byte(`{"q":"hi"}`)
	return r
}

func TestBuildAgentRunPod_LoopMode(t *testing.T) {
	pod := builders.BuildAgentRunPod(sampleRun(), sampleAgent())
	if pod.Name != "run-001" || pod.Namespace != "tenant-a" {
		t.Errorf("name/ns wrong: %s/%s", pod.Namespace, pod.Name)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", pod.Spec.RestartPolicy)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d", len(pod.Spec.Containers))
	}
	if pod.Spec.Containers[0].Name != "agent" {
		t.Errorf("loop container name = %s", pod.Spec.Containers[0].Name)
	}
}

func TestBuildAgentRunPod_HarnessMode_Image(t *testing.T) {
	a := sampleAgent()
	a.Spec.Mode = pure.ModeHarness
	a.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessClaudeCode, Image: "myreg/claude:1"}
	pod := builders.BuildAgentRunPod(sampleRun(), a)
	if pod.Spec.Containers[0].Name != "harness" {
		t.Errorf("harness container name = %s", pod.Spec.Containers[0].Name)
	}
	if pod.Spec.Containers[0].Image != "myreg/claude:1" {
		t.Errorf("image override ignored: %s", pod.Spec.Containers[0].Image)
	}
}

func TestBuildAgentRunPod_AgentFSVolume(t *testing.T) {
	a := sampleAgent()
	a.Spec.Mode = pure.ModeHarness
	a.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessClaudeCode}
	a.Spec.Storage = &pure.StorageSpec{
		Kind:    pure.StorageAgentFS,
		AgentFS: &pure.AgentFSSpec{SizeGiB: 5, MountPath: "/var/agentfs"},
	}
	pod := builders.BuildAgentRunPod(sampleRun(), a)
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "agentfs" {
			found = true
		}
	}
	if !found {
		t.Error("agentfs volume not declared")
	}
	mount := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == "agentfs" && m.MountPath == "/var/agentfs" {
			mount = true
		}
	}
	if !mount {
		t.Error("agentfs mount missing on harness container")
	}
}

func TestAgentRunReconciler_MarkRunning(t *testing.T) {
	r := &AgentRunReconciler{}
	run := sampleRun()
	r.markRunning(run)
	if run.Status.State != pure.PhaseRunning {
		t.Errorf("state=%s", run.Status.State)
	}
	if run.Status.StartedAt == nil {
		t.Error("StartedAt not set")
	}
	// Idempotent: a second markRunning should not overwrite StartedAt.
	first := *run.Status.StartedAt
	time.Sleep(2 * time.Millisecond)
	r.markRunning(run)
	if !run.Status.StartedAt.Equal(&first) {
		t.Errorf("StartedAt drifted on idempotent mark")
	}
}

// newRunReconcilerForTest builds an in-memory AgentRun reconciler, optionally
// intercepting client calls (e.g. to inject a status-update conflict).
func newRunReconcilerForTest(t *testing.T, fns interceptor.Funcs, initial ...client.Object) *AgentRunReconciler {
	t.Helper()
	sch := runtime.NewScheme()
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("amv1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(initial...).
		WithStatusSubresource(&amv1.AgentRun{}).
		WithInterceptorFuncs(fns).
		Build()
	return &AgentRunReconciler{Client: c, Scheme: sch}
}

// A status-update conflict is benign cache lag, not a failure: updateRunStatus
// must requeue without surfacing an error (otherwise the controller logs a
// noisy "Reconciler error" on every Pod-watch/poll race).
func TestAgentRunReconciler_updateRunStatus_ConflictRequeues(t *testing.T) {
	run := sampleRun()
	conflict := apierrors.NewConflict(
		schema.GroupResource{Group: "runtime.agents.smol-agents.ai", Resource: "agentruns"},
		run.Name, errors.New("the object has been modified"))
	r := newRunReconcilerForTest(t, interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return conflict
		},
	}, run)

	res, err := r.updateRunStatus(context.Background(), run, ctrl.Result{RequeueAfter: 5 * time.Second})
	if err != nil {
		t.Fatalf("conflict must not surface as an error, got %v", err)
	}
	if !res.Requeue {
		t.Errorf("conflict should requeue, got %+v", res)
	}
}

// A successful write returns the caller's intended Result verbatim.
func TestAgentRunReconciler_updateRunStatus_SuccessReturnsResult(t *testing.T) {
	run := sampleRun()
	r := newRunReconcilerForTest(t, interceptor.Funcs{}, run)
	want := ctrl.Result{RequeueAfter: 5 * time.Second}
	res, err := r.updateRunStatus(context.Background(), run, want)
	if err != nil {
		t.Fatalf("updateRunStatus: %v", err)
	}
	if res != want {
		t.Errorf("res = %+v, want %+v", res, want)
	}
}

// A non-conflict error must still surface — conflict tolerance must not swallow
// real failures.
func TestAgentRunReconciler_updateRunStatus_NonConflictErrorSurfaces(t *testing.T) {
	run := sampleRun()
	r := newRunReconcilerForTest(t, interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return errors.New("boom")
		},
	}, run)
	if _, err := r.updateRunStatus(context.Background(), run, ctrl.Result{}); err == nil {
		t.Error("a non-conflict error must surface, not be swallowed")
	}
}

func TestAgentRunReconciler_MarkTerminal(t *testing.T) {
	r := &AgentRunReconciler{}
	run := sampleRun()
	r.markTerminal(run, pure.PhaseCompleted, "")
	if run.Status.State != pure.PhaseCompleted {
		t.Errorf("state=%s", run.Status.State)
	}
	if run.Status.EndedAt == nil {
		t.Error("EndedAt not set")
	}
}

// markTerminal must own TerminationReason — without this, a stale
// "Pod is Pending" hint left by markPending was surviving into Completed.
func TestAgentRunReconciler_MarkTerminal_ClearsStaleReason(t *testing.T) {
	r := &AgentRunReconciler{}
	run := sampleRun()
	r.markPending(run, "PodPending", "Pod is Pending") // leaves a stale reason
	if run.Status.TerminationReason != "Pod is Pending" {
		t.Fatalf("setup: terminationReason=%q", run.Status.TerminationReason)
	}
	r.markTerminal(run, pure.PhaseCompleted, "")
	if run.Status.TerminationReason != "" {
		t.Errorf("markTerminal(\"\") should clear stale reason, got %q", run.Status.TerminationReason)
	}
}

// The runtime's own reason ("budget:tokens" for an Expired run that still
// exits 0) is the most specific signal and must win over whatever pod-level
// reason markTerminal left behind.
// M3.12: a Hermes run that belongs to an AgentSession gets a stable
// HERMES_SESSION_ID (sess-<session UID>) + persistent session policy injected
// into a COPY of the agent; the stored Agent is never mutated.
func TestHermesSessionAgent_InjectsSessionID(t *testing.T) {
	session := &amv1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "sess-1", Namespace: "tenant-a", UID: "uid-xyz"}}
	r := newRunReconcilerForTest(t, interceptor.Funcs{}, session)
	hermes := harnessAgent("alice", "tenant-a") // HarnessHermes

	run := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a"}}
	run.Spec.SessionRef = "sess-1"
	out := r.hermesSessionAgent(context.Background(), run, hermes)
	if out.Spec.Harness.SessionPolicy != pure.SessionPersistent {
		t.Errorf("sessionPolicy not set to persistent")
	}
	var got string
	for _, e := range out.Spec.Harness.Env {
		if e.Name == "HERMES_SESSION_ID" {
			got = e.Value
		}
	}
	if got != "sess-uid-xyz" {
		t.Errorf("HERMES_SESSION_ID = %q, want sess-uid-xyz", got)
	}
	if len(hermes.Spec.Harness.Env) != 0 {
		t.Errorf("original agent env mutated: %+v", hermes.Spec.Harness.Env)
	}

	noSess := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a"}}
	if r.hermesSessionAgent(context.Background(), noSess, hermes) != hermes {
		t.Errorf("no sessionRef must return the agent unchanged")
	}
}

// M4.18: a pod killed by activeDeadlineSeconds carries the reason at the POD
// level — terminationReason must surface it (pod:DeadlineExceeded), preferring
// the pod-level reason over container status, and falling back to pod:Failed.
func TestTerminationReason_PrefersPodLevel(t *testing.T) {
	deadline := &corev1.Pod{Status: corev1.PodStatus{Reason: "DeadlineExceeded"}}
	if got := terminationReason(deadline); got != "pod:DeadlineExceeded" {
		t.Errorf("deadline pod → %q, want pod:DeadlineExceeded", got)
	}
	container := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}},
	}}}
	if got := terminationReason(container); got != "pod:OOMKilled" {
		t.Errorf("container-terminated pod → %q, want pod:OOMKilled", got)
	}
	if got := terminationReason(&corev1.Pod{}); got != "pod:Failed" {
		t.Errorf("bare failed pod → %q, want pod:Failed", got)
	}
}

func TestAgentRunReconciler_FoldRunResult_RuntimeReasonWins(t *testing.T) {
	r := newRunReconcilerForTest(t, interceptor.Funcs{})
	run := sampleRun()
	r.markTerminal(run, pure.PhaseCompleted, "") // pod said Succeeded, no reason
	pod := runPodWithTerminationMessage(agentruntime.RunResult{
		Phase:             pure.PhaseExpired,
		TerminationReason: "budget:tokens",
		Usage:             pure.Usage{Steps: 1, Tokens: 16102},
	})
	r.foldRunResult(context.Background(), run, pod)
	if run.Status.State != pure.PhaseExpired {
		t.Errorf("state should be refined to Expired, got %q", run.Status.State)
	}
	if run.Status.TerminationReason != "budget:tokens" {
		t.Errorf("terminationReason = %q, want budget:tokens", run.Status.TerminationReason)
	}
	if run.Status.Usage.Tokens != 16102 {
		t.Errorf("usage not folded: tokens=%d", run.Status.Usage.Tokens)
	}
}

// A runtime error wins over both any prior reason and over the runtime's own
// TerminationReason (an Error is more diagnostic than the bare reason).
func TestAgentRunReconciler_FoldRunResult_ErrorWins(t *testing.T) {
	r := newRunReconcilerForTest(t, interceptor.Funcs{})
	run := sampleRun()
	r.markTerminal(run, pure.PhaseFailed, "pod:Error")
	pod := runPodWithTerminationMessage(agentruntime.RunResult{
		Phase:             pure.PhaseFailed,
		TerminationReason: "harness:timeout",
		Error:             "harness: http 502: upstream refused",
	})
	r.foldRunResult(context.Background(), run, pod)
	if run.Status.TerminationReason != "harness: http 502: upstream refused" {
		t.Errorf("error should win, got %q", run.Status.TerminationReason)
	}
}

func getRun(t *testing.T, r *AgentRunReconciler, ns, name string) *amv1.AgentRun {
	t.Helper()
	got := &amv1.AgentRun{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, got); err != nil {
		t.Fatalf("get run: %v", err)
	}
	return got
}

func runPodExists(r *AgentRunReconciler, ns, name string) bool {
	return r.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &corev1.Pod{}) == nil
}

// M1.12: the per-tenant concurrency gate holds a run Pending when the namespace
// (or Agent) is at its Running-runs cap, and admits otherwise.
func mkRun(name, ns, ref string) *amv1.AgentRun {
	r := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	r.Spec.AgentRef = ref
	r.Spec.Input = []byte(`{}`)
	return r
}

func TestAdmitRunConcurrency_NamespaceCap(t *testing.T) {
	quota := &amv1.AgentRunQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "tenant-a"},
		Spec:       pure.AgentRunQuotaSpec{MaxConcurrentRuns: 2},
	}
	rec := newRunReconcilerForTest(t, interceptor.Funcs{}, quota, mkRun("run-a", "tenant-a", "alice"), mkRun("run-b", "tenant-a", "bob"))
	for _, n := range []string{"run-a", "run-b"} { // seed both Running
		got := getRun(t, rec, "tenant-a", n)
		got.Status.State = pure.PhaseRunning
		if err := rec.Status().Update(context.Background(), got); err != nil {
			t.Fatalf("seed running: %v", err)
		}
	}
	newRun := mkRun("run-c", "tenant-a", "carol")
	handled, _, _ := rec.admitRunConcurrency(context.Background(), newRun, harnessAgent("carol", "tenant-a"))
	if !handled || newRun.Status.State != pure.PhasePending {
		t.Fatalf("at-cap run must be held Pending; handled=%v state=%q", handled, newRun.Status.State)
	}
	if newRun.Status.TerminationReason != "namespace at concurrency cap 2" {
		t.Errorf("reason = %q", newRun.Status.TerminationReason)
	}
}

func TestAdmitRunConcurrency_UnderCapAdmits(t *testing.T) {
	quota := &amv1.AgentRunQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "q", Namespace: "tenant-a"},
		Spec:       pure.AgentRunQuotaSpec{MaxConcurrentRuns: 5},
	}
	rec := newRunReconcilerForTest(t, interceptor.Funcs{}, quota, mkRun("run-a", "tenant-a", "alice"))
	got := getRun(t, rec, "tenant-a", "run-a")
	got.Status.State = pure.PhaseRunning
	_ = rec.Status().Update(context.Background(), got)

	handled, _, _ := rec.admitRunConcurrency(context.Background(), mkRun("run-b", "tenant-a", "bob"), harnessAgent("bob", "tenant-a"))
	if handled {
		t.Fatalf("under cap must admit (handled=false)")
	}
}

func TestAdmitRunConcurrency_PerAgentCap(t *testing.T) {
	rec := newRunReconcilerForTest(t, interceptor.Funcs{}, mkRun("run-a", "tenant-a", "alice"))
	got := getRun(t, rec, "tenant-a", "run-a")
	got.Status.State = pure.PhaseRunning
	_ = rec.Status().Update(context.Background(), got)

	agent := harnessAgent("alice", "tenant-a")
	agent.Spec.MaxConcurrentRuns = 1 // alice already has 1 Running
	newRun := mkRun("run-b", "tenant-a", "alice")
	handled, _, _ := rec.admitRunConcurrency(context.Background(), newRun, agent)
	if !handled || newRun.Status.State != pure.PhasePending {
		t.Fatalf("per-agent cap must hold the second alice run; handled=%v state=%q", handled, newRun.Status.State)
	}
}

// M5.3: a gated run parks in RequiresAction (no pod, no cost) until a token-
// matched approval lets it proceed; deny → Cancelled; stale token ignored; TTL
// → Expired.
func TestPreRunApproval_GateThenApprove(t *testing.T) {
	agent := harnessAgent("alice", "tenant-a")
	agent.Spec.Approval = &pure.ApprovalPolicy{RequireApprovalBeforeRun: true, ApprovalTimeoutSeconds: 3600}
	r := newRunReconcilerForTest(t, interceptor.Funcs{}, agent, sampleRun())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "run-001"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	got := getRun(t, r, "tenant-a", "run-001")
	if got.Status.State != pure.PhaseRequiresAction {
		t.Fatalf("want RequiresAction, got %q", got.Status.State)
	}
	if got.Status.PendingAction == nil || got.Status.PendingAction.Token == "" || got.Status.PendingAction.Kind != "pre-run" {
		t.Fatalf("pending token not minted: %+v", got.Status.PendingAction)
	}
	if runPodExists(r, "tenant-a", "run-001") {
		t.Fatalf("no pod must exist while awaiting approval")
	}

	got.Spec.Decision = &pure.Decision{Token: got.Status.PendingAction.Token, Approve: true, DecidedBy: "ops"}
	if err := r.Update(context.Background(), got); err != nil {
		t.Fatalf("patch decision: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	after := getRun(t, r, "tenant-a", "run-001")
	if after.Status.State == pure.PhaseRequiresAction {
		t.Fatalf("approval should clear the gate; still RequiresAction")
	}
	if after.Status.PendingAction != nil {
		t.Errorf("PendingAction should be cleared after approval, got %+v", after.Status.PendingAction)
	}
}

func TestPreRunApproval_Deny(t *testing.T) {
	agent := harnessAgent("alice", "tenant-a")
	agent.Spec.Approval = &pure.ApprovalPolicy{RequireApprovalBeforeRun: true, ApprovalTimeoutSeconds: 3600}
	r := newRunReconcilerForTest(t, interceptor.Funcs{}, agent, sampleRun())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "run-001"}}

	_, _ = r.Reconcile(context.Background(), req)
	got := getRun(t, r, "tenant-a", "run-001")
	got.Spec.Decision = &pure.Decision{Token: got.Status.PendingAction.Token, Approve: false, Reason: "nope"}
	if err := r.Update(context.Background(), got); err != nil {
		t.Fatalf("patch decision: %v", err)
	}
	_, _ = r.Reconcile(context.Background(), req)
	after := getRun(t, r, "tenant-a", "run-001")
	if after.Status.State != pure.PhaseCancelled {
		t.Fatalf("deny → want Cancelled, got %q", after.Status.State)
	}
}

func TestPreRunApproval_StaleTokenIgnored(t *testing.T) {
	agent := harnessAgent("alice", "tenant-a")
	agent.Spec.Approval = &pure.ApprovalPolicy{RequireApprovalBeforeRun: true, ApprovalTimeoutSeconds: 3600}
	r := newRunReconcilerForTest(t, interceptor.Funcs{}, agent, sampleRun())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "run-001"}}

	_, _ = r.Reconcile(context.Background(), req)
	got := getRun(t, r, "tenant-a", "run-001")
	got.Spec.Decision = &pure.Decision{Token: "wrong-token", Approve: true}
	if err := r.Update(context.Background(), got); err != nil {
		t.Fatalf("patch: %v", err)
	}
	_, _ = r.Reconcile(context.Background(), req)
	after := getRun(t, r, "tenant-a", "run-001")
	if after.Status.State != pure.PhaseRequiresAction {
		t.Fatalf("stale token must keep the run parked, got %q", after.Status.State)
	}
}

func TestPreRunApproval_ExpireOnTTL(t *testing.T) {
	agent := harnessAgent("alice", "tenant-a")
	agent.Spec.Approval = &pure.ApprovalPolicy{RequireApprovalBeforeRun: true, ApprovalTimeoutSeconds: 1}
	r := newRunReconcilerForTest(t, interceptor.Funcs{}, agent, sampleRun())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "run-001"}}

	// Seed an already-old pending action (the fake client may not persist an
	// initial Status, so set it explicitly).
	seed := getRun(t, r, "tenant-a", "run-001")
	seed.Status.State = pure.PhaseRequiresAction
	seed.Status.PendingAction = &pure.PendingAction{
		Kind: "pre-run", Token: "tok1",
		RequestedAt: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
	}
	if err := r.Status().Update(context.Background(), seed); err != nil {
		t.Fatalf("seed status: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	after := getRun(t, r, "tenant-a", "run-001")
	if after.Status.State != pure.PhaseExpired {
		t.Fatalf("past-TTL approval → want Expired, got %q", after.Status.State)
	}
}

// M1.4: a namespace RedactionPolicy masks the folded Status.Output and any
// secret in a Step's tool-call result; with no policy the fold is byte-identical
// (zero-overhead fast path). RedactJSON re-marshals via map[string]any, so key
// order is sorted and the masked output is deterministic.
func TestAgentRunReconciler_FoldRunResult_Redaction(t *testing.T) {
	policy := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "redact", Namespace: "tenant-a"},
		Spec:       pure.AgentPolicySpec{Redaction: &pure.RedactionPolicy{Patterns: []string{`sk-[a-z0-9]+`}}},
	}
	rr := agentruntime.RunResult{
		Phase:  pure.PhaseCompleted,
		Output: json.RawMessage(`{"key":"sk-deadbeef","n":1}`),
		Steps: []pure.Step{{Index: 0, Kind: pure.StepToolCall, ToolCalls: []pure.ToolCallRecord{
			{Tool: "fetch", Result: json.RawMessage(`{"token":"sk-secret99"}`)},
		}}},
	}

	// Policy present → secrets masked.
	r := newRunReconcilerForTest(t, interceptor.Funcs{}, policy)
	run := sampleRun()
	r.foldRunResult(context.Background(), run, runPodWithTerminationMessage(rr))
	if got := string(run.Status.Output); got != `{"key":"[REDACTED]","n":1}` {
		t.Errorf("Output not redacted: %s", got)
	}
	if got := string(run.Status.Steps[0].ToolCalls[0].Result); got != `{"token":"[REDACTED]"}` {
		t.Errorf("Step tool-call result not redacted: %s", got)
	}

	// No policy → byte-identical fold.
	r2 := newRunReconcilerForTest(t, interceptor.Funcs{})
	run2 := sampleRun()
	r2.foldRunResult(context.Background(), run2, runPodWithTerminationMessage(rr))
	if got := string(run2.Status.Output); got != `{"key":"sk-deadbeef","n":1}` {
		t.Errorf("no-policy output must be byte-identical: %s", got)
	}
}

// M2.2: the run's trace summary folds into Status.Trace.
func TestAgentRunReconciler_FoldRunResult_Trace(t *testing.T) {
	r := newRunReconcilerForTest(t, interceptor.Funcs{})
	run := sampleRun()
	pod := runPodWithTerminationMessage(agentruntime.RunResult{
		Phase: pure.PhaseCompleted,
		Trace: &pure.TraceSummary{StepCount: 3, ToolCallCount: 5, Truncated: true},
	})
	r.foldRunResult(context.Background(), run, pod)
	if run.Status.Trace == nil || run.Status.Trace.StepCount != 3 || run.Status.Trace.ToolCallCount != 5 || !run.Status.Trace.Truncated {
		t.Fatalf("Status.Trace = %+v, want {3,5,truncated}", run.Status.Trace)
	}
}

// A clean RunResult (no Error, no TerminationReason) must not clobber what
// markTerminal already set — for a normal Completed run that's the empty
// string, and the status should land empty.
func TestAgentRunReconciler_FoldRunResult_CleanSuccessLeavesEmpty(t *testing.T) {
	r := newRunReconcilerForTest(t, interceptor.Funcs{})
	run := sampleRun()
	r.markPending(run, "PodPending", "Pod is Pending")
	r.markTerminal(run, pure.PhaseCompleted, "")
	pod := runPodWithTerminationMessage(agentruntime.RunResult{
		Phase:  pure.PhaseCompleted,
		Output: json.RawMessage(`{"answer":42}`),
		Steps:  []pure.Step{{Index: 0, Kind: pure.StepFinal, TokensIn: 60, TokensOut: 40}},
		Usage:  pure.Usage{Steps: 1, Tokens: 100},
	})
	r.foldRunResult(context.Background(), run, pod)
	if run.Status.TerminationReason != "" {
		t.Errorf("clean success should leave empty terminationReason, got %q", run.Status.TerminationReason)
	}
	if string(run.Status.Output) != `{"answer":42}` {
		t.Errorf("output not folded: %s", run.Status.Output)
	}
	if len(run.Status.Steps) != 1 || run.Status.Steps[0].Kind != pure.StepFinal {
		t.Errorf("steps not folded into status: %+v", run.Status.Steps)
	}
}
