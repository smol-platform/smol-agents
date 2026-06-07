package invokers

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// fanoutFake gives each created child a UNIQUE name and, on creation, marks it
// Completed with status.output = its spec.input (so the reducer is observable),
// usage {tokens:10, toolCalls:1}. A child whose input object has "fail":true ends
// Failed — a deterministic, concurrency-safe way to test partial failure.
type fanoutFake struct {
	client.Client
	mu      sync.Mutex
	store   map[string]*unstructured.Unstructured
	created int
}

func (c *fanoutFake) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := obj.(*unstructured.Unstructured)
	c.created++
	u.SetName(u.GetGenerateName() + strconv.Itoa(c.created))
	input, _, _ := unstructured.NestedFieldCopy(u.Object, "spec", "input")
	state := string(v1.PhaseCompleted)
	if m, ok := input.(map[string]any); ok {
		if f, _ := m["fail"].(bool); f {
			state = string(v1.PhaseFailed)
		}
	}
	_ = unstructured.SetNestedField(u.Object, state, "status", "state")
	if state == string(v1.PhaseCompleted) {
		_ = unstructured.SetNestedField(u.Object, input, "status", "output")
		_ = unstructured.SetNestedField(u.Object, int64(10), "status", "usage", "tokens")
		_ = unstructured.SetNestedField(u.Object, int64(1), "status", "usage", "toolCalls")
	}
	if c.store == nil {
		c.store = map[string]*unstructured.Unstructured{}
	}
	c.store[u.GetName()] = u.DeepCopy()
	return nil
}

func (c *fanoutFake) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.store[key.Name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "agentruns"}, key.Name)
	}
	u.DeepCopyInto(obj.(*unstructured.Unstructured))
	return nil
}

func (c *fanoutFake) Delete(_ context.Context, _ client.Object, _ ...client.DeleteOption) error {
	return nil
}

func fanoutTool(reduce v1.FanoutReducer, maxParallel int32) v1.Tool {
	return v1.Tool{Name: "fan", Spec: v1.ToolSpec{Kind: v1.ToolFanout, Fanout: &v1.FanoutTargetSpec{
		Ref: v1.ToolRef{Name: "worker"}, Reduce: reduce, MaxParallel: maxParallel,
	}}}
}

func newFanout(c client.Client) *FanoutInvoker {
	return &FanoutInvoker{Client: c, Namespace: "t", MaxDepth: 4, MaxWidth: 64, Poll: time.Millisecond}
}

func TestFanout_ConcatFoldAndUsage(t *testing.T) {
	fc := &fanoutFake{}
	obs, err := newFanout(fc).Invoke(context.Background(), fanoutTool(v1.FanoutConcat, 0),
		json.RawMessage(`{"items":[{"k":1},{"k":2},{"k":3}]}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var out struct {
		Results []map[string]int `json:"results"`
		Errors  int              `json:"errors"`
	}
	if err := json.Unmarshal(obs.Output, &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, obs.Output)
	}
	if len(out.Results) != 3 || out.Errors != 0 {
		t.Fatalf("concat: want 3 results 0 errors, got %+v", out)
	}
	// Field-wise usage: 3 children × {10 tokens, 1 call}.
	if obs.Tokens != 30 || obs.ToolCalls != 3 {
		t.Fatalf("usage roll-up: want 30/3, got %d/%d", obs.Tokens, obs.ToolCalls)
	}
	if fc.created != 3 {
		t.Fatalf("want 3 children created, got %d", fc.created)
	}
}

func TestFanout_MergeFold(t *testing.T) {
	fc := &fanoutFake{}
	obs, err := newFanout(fc).Invoke(context.Background(), fanoutTool(v1.FanoutMerge, 0),
		json.RawMessage(`{"items":[{"a":1},{"b":2},{"c":3}]}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var out struct {
		Merged map[string]int `json:"merged"`
		Errors int            `json:"errors"`
	}
	_ = json.Unmarshal(obs.Output, &out)
	if len(out.Merged) != 3 || out.Merged["a"] != 1 || out.Merged["c"] != 3 {
		t.Fatalf("merge: want {a,b,c}, got %+v", out)
	}
}

func TestFanout_PartialFailureSurfaced(t *testing.T) {
	fc := &fanoutFake{}
	// Middle child fails; concat must surface the survivors + an error count.
	obs, err := newFanout(fc).Invoke(context.Background(), fanoutTool(v1.FanoutConcat, 0),
		json.RawMessage(`{"items":[{"k":1},{"fail":true},{"k":3}]}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var out struct {
		Results []map[string]any `json:"results"`
		Errors  int              `json:"errors"`
	}
	_ = json.Unmarshal(obs.Output, &out)
	if len(out.Results) != 2 || out.Errors != 1 {
		t.Fatalf("partial: want 2 results 1 error, got %+v", out)
	}
}

func TestFanout_AllFail(t *testing.T) {
	fc := &fanoutFake{}
	if _, err := newFanout(fc).Invoke(context.Background(), fanoutTool(v1.FanoutConcat, 0),
		json.RawMessage(`{"items":[{"fail":true},{"fail":true}]}`)); err == nil {
		t.Fatal("all-children-fail must return an error")
	}
}

func TestFanout_FirstSuccess(t *testing.T) {
	fc := &fanoutFake{}
	obs, err := newFanout(fc).Invoke(context.Background(), fanoutTool(v1.FanoutFirstSuccess, 0),
		json.RawMessage(`{"items":[{"k":1},{"k":2}]}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var m map[string]int
	if err := json.Unmarshal(obs.Output, &m); err != nil || m["k"] == 0 {
		t.Fatalf("first-success must return a single child's output, got %s", obs.Output)
	}
}

func TestFanout_FailClosedAndGuards(t *testing.T) {
	// FANOUT_MAX_WIDTH absent (MaxWidth=0) → fail-closed.
	noWidth := &FanoutInvoker{Client: &fanoutFake{}, Namespace: "t", MaxDepth: 4, MaxWidth: 0, Poll: time.Millisecond}
	if _, err := noWidth.Invoke(context.Background(), fanoutTool(v1.FanoutConcat, 0), json.RawMessage(`{"items":[{}]}`)); err == nil {
		t.Fatal("MaxWidth=0 must fail closed")
	}
	// items exceed MaxWidth → refused (hard clamp), no children created.
	fc := &fanoutFake{}
	clamp := &FanoutInvoker{Client: fc, Namespace: "t", MaxDepth: 4, MaxWidth: 2, Poll: time.Millisecond}
	if _, err := clamp.Invoke(context.Background(), fanoutTool(v1.FanoutConcat, 0), json.RawMessage(`{"items":[{},{},{}]}`)); err == nil {
		t.Fatal("items > MaxWidth must be refused")
	}
	if fc.created != 0 {
		t.Fatalf("clamp must create no children, got %d", fc.created)
	}
	// Depth guard: at the ceiling → refuse, no children.
	fc2 := &fanoutFake{}
	deep := &FanoutInvoker{Client: fc2, Namespace: "t", Depth: 4, MaxDepth: 4, MaxWidth: 64, Poll: time.Millisecond}
	if _, err := deep.Invoke(context.Background(), fanoutTool(v1.FanoutConcat, 0), json.RawMessage(`{"items":[{}]}`)); err == nil {
		t.Fatal("depth at ceiling must refuse")
	}
	if fc2.created != 0 {
		t.Fatalf("depth guard must create no children, got %d", fc2.created)
	}
}
