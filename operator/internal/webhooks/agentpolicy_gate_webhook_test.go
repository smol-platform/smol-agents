package webhooks

import (
	"context"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func gateWith(t *testing.T, objs ...client.Object) *agentPolicyGate {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	return &agentPolicyGate{client: c}
}

func TestAgentPolicyGate_AgentProvider(t *testing.T) {
	policy := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "t"},
		Spec:       pure.AgentPolicySpec{AllowedProviders: []string{"openai"}},
	}
	g := gateWith(t, policy)

	bad := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	bad.Spec.Model = pure.ModelRef{ProviderRef: "anthropic", Name: "m"}
	if err := g.checkAgent(context.Background(), bad); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("disallowed provider must be Invalid, got %v", err)
	}
	good := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	good.Spec.Model = pure.ModelRef{ProviderRef: "openai", Name: "m"}
	if err := g.checkAgent(context.Background(), good); err != nil {
		t.Fatalf("conforming provider must pass: %v", err)
	}
}

// gateWithListError builds an agentPolicyGate whose client.List always fails,
// simulating an apiserver hiccup while reading AgentPolicy.
func gateWithListError(t *testing.T) *agentPolicyGate {
	t.Helper()
	sch := runtime.NewScheme()
	if err := amv1.AddToScheme(sch); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	base := fake.NewClientBuilder().WithScheme(sch).Build()
	ic := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			return fmt.Errorf("simulated apiserver hiccup")
		},
	})
	return &agentPolicyGate{client: ic}
}

// knative-agents-2mi: a List error reading AgentPolicy must fail CLOSED — the
// validator denies with a retryable (non-Invalid) error rather than silently
// admitting an Agent/AgentRun the namespace policy might have rejected.
func TestAgentPolicyGate_ListError_Agent_DeniesNotInvalid(t *testing.T) {
	g := gateWithListError(t)
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	a.Spec.Model = pure.ModelRef{ProviderRef: "anything", Name: "m"}

	err := g.checkAgent(context.Background(), a)
	if err == nil {
		t.Fatal("List error must deny the Agent, got nil (fail-open)")
	}
	if apierrors.IsInvalid(err) {
		t.Fatalf("List error must not surface as a policy-violation Invalid, got %v", err)
	}
}

func TestAgentPolicyGate_ListError_Run_DeniesNotInvalid(t *testing.T) {
	g := gateWithListError(t)
	run := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "t"}}
	run.Spec.AgentRef = "a"
	run.Spec.BudgetOverride = &pure.Budget{MaxTokens: 500, MaxSteps: 10, MaxWallClockSeconds: 60, MaxToolCalls: 5}

	err := g.checkRun(context.Background(), run)
	if err == nil {
		t.Fatal("List error must deny the AgentRun, got nil (fail-open)")
	}
	if apierrors.IsInvalid(err) {
		t.Fatalf("List error must not surface as a policy-violation Invalid, got %v", err)
	}
}

func TestAgentPolicyGate_FailOpenNoPolicies(t *testing.T) {
	g := gateWith(t) // no policies → fail open
	a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	a.Spec.Model = pure.ModelRef{ProviderRef: "anything", Name: "m"}
	if err := g.checkAgent(context.Background(), a); err != nil {
		t.Fatalf("no policies must fail open, got %v", err)
	}
}

func TestAgentPolicyGate_LoopToolKinds(t *testing.T) {
	httpTool := &amv1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "ht", Namespace: "t"}, Spec: pure.ToolSpec{Kind: pure.ToolHTTP}}
	fnTool := &amv1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "fn", Namespace: "t"}, Spec: pure.ToolSpec{Kind: pure.ToolFunction}}
	g := gateWith(t, httpTool, fnTool)

	// loop-mode agent referencing a function-kind tool (no loop invoker) → warns
	// at admission (does NOT reject; the reconciler is the enforcement and flips
	// the admitted Agent to Failed/ToolKindUnsupported).
	bad := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	bad.Spec.Tools = []pure.ToolRef{{Name: "fn"}}
	if warns := g.checkLoopToolKinds(context.Background(), bad); len(warns) != 1 {
		t.Fatalf("function-kind loop tool must warn (1 warning), got %v", warns)
	}
	// http-kind tool → no warning
	good := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	good.Spec.Tools = []pure.ToolRef{{Name: "ht"}}
	if warns := g.checkLoopToolKinds(context.Background(), good); len(warns) != 0 {
		t.Fatalf("http-kind loop tool must not warn: %v", warns)
	}
	// harness mode → loop-tool-kind check is skipped even with a function tool
	harness := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	harness.Spec.Mode = pure.ModeHarness
	harness.Spec.Tools = []pure.ToolRef{{Name: "fn"}}
	if warns := g.checkLoopToolKinds(context.Background(), harness); len(warns) != 0 {
		t.Fatalf("harness mode must skip loop-tool-kind check: %v", warns)
	}
	// dangling ref → not judged here (reconciler handles existence)
	dangling := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	dangling.Spec.Tools = []pure.ToolRef{{Name: "nope"}}
	if warns := g.checkLoopToolKinds(context.Background(), dangling); len(warns) != 0 {
		t.Fatalf("dangling tool ref must not warn at this gate: %v", warns)
	}
}

func TestAgentPolicyGate_RunBudgetOverride(t *testing.T) {
	policy := &amv1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "t"},
		Spec:       pure.AgentPolicySpec{MaxBudget: &pure.Budget{MaxTokens: 1000, MaxSteps: 10, MaxWallClockSeconds: 60, MaxToolCalls: 5}},
	}
	g := gateWith(t, policy)

	over := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "t"}}
	over.Spec.AgentRef = "a"
	over.Spec.BudgetOverride = &pure.Budget{MaxTokens: 2000, MaxSteps: 10, MaxWallClockSeconds: 60, MaxToolCalls: 5}
	if err := g.checkRun(context.Background(), over); err == nil || !apierrors.IsInvalid(err) {
		t.Fatalf("over-cap budgetOverride must be Invalid, got %v", err)
	}
	within := &amv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "t"}}
	within.Spec.AgentRef = "a"
	within.Spec.BudgetOverride = &pure.Budget{MaxTokens: 500, MaxSteps: 10, MaxWallClockSeconds: 60, MaxToolCalls: 5}
	if err := g.checkRun(context.Background(), within); err != nil {
		t.Fatalf("within-cap override must pass: %v", err)
	}
}

func TestAgentPolicyGate_ClaudeWriteRuntime(t *testing.T) {
	mk := func(kind pure.HarnessKind, cli *pure.HarnessCLISpec, runtimeClass string) *amv1.Agent {
		a := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
		a.Spec.Harness = &pure.HarnessSpec{Kind: kind, CLI: cli}
		a.Spec.Sandbox = pure.SandboxSpec{RuntimeClass: runtimeClass}
		return a
	}
	g := &agentPolicyGate{defaultRunClass: "kata-fc"}

	// claude + Write tool on runc → warn
	if w := g.checkClaudeWriteRuntime(mk(pure.HarnessClaudeCode, &pure.HarnessCLISpec{AllowedTools: []string{"Write"}}, "runc")); len(w) != 1 {
		t.Fatalf("claude+Write on runc must warn, got %v", w)
	}
	// approvalMode acceptEdits on gvisor → warn
	if w := g.checkClaudeWriteRuntime(mk(pure.HarnessClaudeCode, &pure.HarnessCLISpec{ApprovalMode: "acceptEdits"}, "gvisor")); len(w) != 1 {
		t.Fatalf("claude acceptEdits on gvisor must warn, got %v", w)
	}
	// kata explicit → no warn
	if w := g.checkClaudeWriteRuntime(mk(pure.HarnessClaudeCode, &pure.HarnessCLISpec{AllowedTools: []string{"Edit"}}, "kata-fc")); len(w) != 0 {
		t.Fatalf("kata must not warn, got %v", w)
	}
	// empty runtimeClass resolves to the operator default (kata-fc) → no warn
	if w := g.checkClaudeWriteRuntime(mk(pure.HarnessClaudeCode, &pure.HarnessCLISpec{AllowedTools: []string{"Write"}}, "")); len(w) != 0 {
		t.Fatalf("default kata must not warn, got %v", w)
	}
	// empty runtimeClass with a runc operator default → warn
	gRunc := &agentPolicyGate{defaultRunClass: "runc"}
	if w := gRunc.checkClaudeWriteRuntime(mk(pure.HarnessClaudeCode, &pure.HarnessCLISpec{AllowedTools: []string{"Write"}}, "")); len(w) != 1 {
		t.Fatalf("runc default must warn, got %v", w)
	}
	// read-only tools on runc → no warn
	if w := g.checkClaudeWriteRuntime(mk(pure.HarnessClaudeCode, &pure.HarnessCLISpec{AllowedTools: []string{"Read", "Grep"}}, "runc")); len(w) != 0 {
		t.Fatalf("read-only must not warn, got %v", w)
	}
	// non-claude harness → no warn
	if w := g.checkClaudeWriteRuntime(mk(pure.HarnessCodex, &pure.HarnessCLISpec{AllowedTools: []string{"Write"}}, "runc")); len(w) != 0 {
		t.Fatalf("codex must not warn, got %v", w)
	}
	// no harness → no warn
	bare := &amv1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "t"}}
	if w := g.checkClaudeWriteRuntime(bare); len(w) != 0 {
		t.Fatalf("loop agent must not warn, got %v", w)
	}
}
