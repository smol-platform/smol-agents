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
