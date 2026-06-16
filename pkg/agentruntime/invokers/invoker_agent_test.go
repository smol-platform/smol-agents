package invokers

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// fakeRunClient is a minimal client.Client that records created AgentRuns and,
// on creation, immediately marks them Completed with a canned output — standing
// in for the run controller so the invoker's create→poll→fold loop is testable
// without a cluster. Only Create + Get are exercised.
type fakeRunClient struct {
	client.Client
	mu        sync.Mutex
	store     map[string]*unstructured.Unstructured
	created   []*unstructured.Unstructured
	output    string // status.output JSON to fold back
	failState string // if set, child ends in this terminal state instead of Completed
	usageTok  int64  // status.usage.tokens to report
	usageCall int64  // status.usage.toolCalls to report
}

func (c *fakeRunClient) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := obj.(*unstructured.Unstructured)
	u.SetName(u.GetGenerateName() + "xyz")
	state := string(v1.PhaseCompleted)
	if c.failState != "" {
		state = c.failState
	}
	_ = unstructured.SetNestedField(u.Object, state, "status", "state")
	if c.output != "" {
		var out any
		_ = json.Unmarshal([]byte(c.output), &out)
		_ = unstructured.SetNestedField(u.Object, out, "status", "output")
	}
	if c.usageTok != 0 {
		_ = unstructured.SetNestedField(u.Object, c.usageTok, "status", "usage", "tokens")
	}
	if c.usageCall != 0 {
		_ = unstructured.SetNestedField(u.Object, c.usageCall, "status", "usage", "toolCalls")
	}
	if c.store == nil {
		c.store = map[string]*unstructured.Unstructured{}
	}
	c.store[u.GetName()] = u.DeepCopy()
	c.created = append(c.created, u.DeepCopy())
	return nil
}

func (c *fakeRunClient) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.store[key.Name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "agentruns"}, key.Name)
	}
	u.DeepCopyInto(obj.(*unstructured.Unstructured))
	return nil
}

// vanishingRunClient creates a child fine but never finds it on Get — modelling a
// child deleted out-of-band (parent GC, manual delete) before it ever reaches a
// terminal state. Exercises the invoker's fail-fast-on-persistent-NotFound guard.
type vanishingRunClient struct {
	client.Client
	created int
}

func (c *vanishingRunClient) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	u := obj.(*unstructured.Unstructured)
	u.SetName(u.GetGenerateName() + "gone")
	c.created++
	return nil
}

func (c *vanishingRunClient) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return apierrors.NewNotFound(schema.GroupResource{Resource: "agentruns"}, key.Name)
}

// neverTerminalRunClient creates a child that stays Running forever — exercises
// the per-call TimeoutSeconds path (M3.5): the invoke must give up + delete it.
type neverTerminalRunClient struct {
	client.Client
	mu      sync.Mutex
	deleted bool
}

func (c *neverTerminalRunClient) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	obj.(*unstructured.Unstructured).SetName("running-child")
	return nil
}

func (c *neverTerminalRunClient) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	_ = unstructured.SetNestedField(obj.(*unstructured.Unstructured).Object, string(v1.PhaseRunning), "status", "state")
	return nil
}

func (c *neverTerminalRunClient) Delete(_ context.Context, _ client.Object, _ ...client.DeleteOption) error {
	c.mu.Lock()
	c.deleted = true
	c.mu.Unlock()
	return nil
}

func (c *neverTerminalRunClient) wasDeleted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deleted
}

func agentTool(target string) v1.Tool {
	t := v1.Tool{}
	t.Name = "delegate"
	t.Spec.Kind = v1.ToolAgent
	t.Spec.Agent = &v1.AgentTargetSpec{Ref: v1.ToolRef{Name: target}}
	return t
}

func TestAgentRunInvoker_CreatesChildAndFolds(t *testing.T) {
	fc := &fakeRunClient{output: `{"result":42}`}
	inv := &AgentRunInvoker{Client: fc, Namespace: "tenant-a", ParentRun: "parent-run", Poll: time.Millisecond}

	obs, err := inv.Invoke(context.Background(), agentTool("child-agent"), json.RawMessage(`{"q":"x"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if string(obs.Output) != `{"result":42}` {
		t.Errorf("folded output = %s, want {\"result\":42}", obs.Output)
	}
	if len(fc.created) != 1 {
		t.Fatalf("expected 1 child created, got %d", len(fc.created))
	}
	child := fc.created[0]
	if got, _, _ := unstructured.NestedString(child.Object, "spec", "agentRef"); got != "child-agent" {
		t.Errorf("child agentRef = %q, want child-agent", got)
	}
	if child.GetLabels()[ParentRunLabel] != "parent-run" {
		t.Errorf("child missing parent label: %v", child.GetLabels())
	}
	if child.GetLabels()[DepthLabel] != "1" {
		t.Errorf("child depth label = %q, want 1", child.GetLabels()[DepthLabel])
	}
	if child.GetNamespace() != "tenant-a" {
		t.Errorf("child namespace = %q, want tenant-a", child.GetNamespace())
	}
}

func TestAgentRunInvoker_RollsUpChildUsage(t *testing.T) {
	fc := &fakeRunClient{output: `{"ok":true}`, usageTok: 150, usageCall: 3}
	inv := &AgentRunInvoker{Client: fc, Namespace: "tenant-a", ParentRun: "p", Poll: time.Millisecond}
	obs, err := inv.Invoke(context.Background(), agentTool("child"), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if obs.Tokens != 150 {
		t.Errorf("child token roll-up = %d, want 150", obs.Tokens)
	}
	if obs.ToolCalls != 3 {
		t.Errorf("child tool-call roll-up = %d, want 3", obs.ToolCalls)
	}
}

func TestAgentRunInvoker_SetsOwnerReference(t *testing.T) {
	fc := &fakeRunClient{output: `{}`}
	inv := &AgentRunInvoker{Client: fc, Namespace: "tenant-a", ParentRun: "parent-run", ParentRunUID: "uid-123", Poll: time.Millisecond}
	if _, err := inv.Invoke(context.Background(), agentTool("child"), nil); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	owners := fc.created[0].GetOwnerReferences()
	if len(owners) != 1 {
		t.Fatalf("expected 1 ownerReference, got %d", len(owners))
	}
	if owners[0].Kind != "AgentRun" || owners[0].Name != "parent-run" || string(owners[0].UID) != "uid-123" {
		t.Errorf("ownerRef = %+v, want AgentRun/parent-run/uid-123", owners[0])
	}
}

func TestAgentRunInvoker_NoOwnerRefWithoutUID(t *testing.T) {
	fc := &fakeRunClient{output: `{}`}
	inv := &AgentRunInvoker{Client: fc, Namespace: "tenant-a", ParentRun: "parent-run", Poll: time.Millisecond}
	if _, err := inv.Invoke(context.Background(), agentTool("child"), nil); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if owners := fc.created[0].GetOwnerReferences(); len(owners) != 0 {
		t.Errorf("no ParentRunUID must mean no ownerRef, got %v", owners)
	}
}

func TestAgentRunInvoker_DepthGuard(t *testing.T) {
	fc := &fakeRunClient{output: `{}`}
	// Depth already at the max → refuse to spawn (fail-closed recursion bound).
	inv := &AgentRunInvoker{Client: fc, Namespace: "tenant-a", Depth: 1, MaxDepth: 1, Poll: time.Millisecond}
	if _, err := inv.Invoke(context.Background(), agentTool("child"), nil); err == nil {
		t.Fatal("expected depth-guard refusal, got nil error")
	}
	if len(fc.created) != 0 {
		t.Errorf("depth guard should not create a child; created %d", len(fc.created))
	}
}

func TestAgentRunInvoker_ChildFailedPropagates(t *testing.T) {
	fc := &fakeRunClient{failState: string(v1.PhaseFailed)}
	inv := &AgentRunInvoker{Client: fc, Namespace: "tenant-a", Poll: time.Millisecond}
	if _, err := inv.Invoke(context.Background(), agentTool("child"), nil); err == nil {
		t.Fatal("expected error when child run Failed")
	}
}

func TestAgentRunInvoker_NoTarget(t *testing.T) {
	fc := &fakeRunClient{}
	inv := &AgentRunInvoker{Client: fc, Namespace: "tenant-a", Poll: time.Millisecond}
	bad := v1.Tool{}
	bad.Spec.Kind = v1.ToolAgent // no Agent target
	if _, err := inv.Invoke(context.Background(), bad, nil); err == nil {
		t.Fatal("expected error for kind=agent tool with no target ref")
	}
}

// M3.5: AgentTargetSpec.TimeoutSeconds bounds a single delegation — a child that
// never terminates makes Invoke give up at the timeout and delete the child.
func TestAgentRunInvoker_PerCallTimeout(t *testing.T) {
	fc := &neverTerminalRunClient{}
	inv := &AgentRunInvoker{Client: fc, Namespace: "tenant-a", ParentRun: "p", Poll: 10 * time.Millisecond}
	tool := agentTool("child")
	tool.Spec.Agent.TimeoutSeconds = 1 // seconds (the spec's granularity)

	start := time.Now()
	done := make(chan error, 1)
	go func() { _, err := inv.Invoke(context.Background(), tool, nil); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a per-call timeout error for a never-terminal child")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Invoke did not honor TimeoutSeconds (still polling)")
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Errorf("returned too early (%v) — the 1s timeout should bound it", time.Since(start))
	}
	if !fc.wasDeleted() {
		t.Error("child must be deleted when the per-call timeout fires")
	}
}

// A child deleted out-of-band (persistent NotFound) must make Invoke return an
// error promptly, not poll until the run deadline. The timeout guards against the
// infinite-poll regression.
func TestAgentRunInvoker_ChildVanishesFailsFast(t *testing.T) {
	fc := &vanishingRunClient{}
	inv := &AgentRunInvoker{Client: fc, Namespace: "tenant-a", ParentRun: "p", Poll: time.Millisecond}

	done := make(chan error, 1)
	go func() {
		_, err := inv.Invoke(context.Background(), agentTool("child"), nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the child vanished, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Invoke did not return — still polling a vanished child (regression)")
	}
	if fc.created != 1 {
		t.Errorf("expected exactly 1 child create attempt, got %d", fc.created)
	}
}

// rv3.1 S4: a team coordinator/member delegating via A2A stamps the team label
// on children (so BuildAgentRunPod injects TEAM_* env); a non-team run does not.
func TestBuildChildRun_TeamPropagation(t *testing.T) {
	withTeam := buildChildRun("ns", "parent", "", "squad", "worker", 0, map[string]any{}, 0)
	if withTeam.GetLabels()[v1.TeamLabel] != "squad" {
		t.Errorf("team child missing team label: %v", withTeam.GetLabels())
	}
	noTeam := buildChildRun("ns", "parent", "", "", "worker", 0, map[string]any{}, 0)
	if _, ok := noTeam.GetLabels()[v1.TeamLabel]; ok {
		t.Errorf("non-team child has a team label: %v", noTeam.GetLabels())
	}
}
