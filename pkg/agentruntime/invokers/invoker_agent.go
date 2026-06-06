package invokers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// agentRunGVK is the AgentRun CRD identity. We talk to it via unstructured so
// the in-pod runtime (root module) needs no dependency on the operator API
// module's typed AgentRun.
var agentRunGVK = schema.GroupVersionKind{
	Group:   "runtime.agents.smol-agents.ai",
	Version: "v1",
	Kind:    "AgentRun",
}

// DepthLabel records how deep in an A2A delegation tree a run is. The invoker
// refuses to spawn a child once the current depth reaches MaxDepth, bounding
// recursion fail-closed. The run pod's depth is injected by the operator from
// this label (env A2A_DEPTH); a top-level run has depth 0.
const DepthLabel = "agents.smol-agents.ai/a2a-depth"

// ParentRunLabel ties a child AgentRun to the run that spawned it (observability
// + a session-tree join key). OwnerReference is set separately by the operator
// when it reconciles the child (it owns GC); the invoker only stamps the label.
const ParentRunLabel = "agents.smol-agents.ai/parent-run"

// AgentRunInvoker implements ToolInvoker for ToolKind=agent (A2A): an LLM tool
// call of kind=agent spawns a CHILD AgentRun for the target agent, blocks until
// the child reaches a terminal state, and returns the child's folded output as
// the observation. It is the runtime half of framework-enhancements.md A1.
//
// Trust model (D1): the child runs in the SAME namespace (never cross-tenant),
// gets its own broker config + SPIFFE id (args carry no secrets), and is bound
// by its own Agent's sandbox + budget. The pod's RBAC (the <agent>-a2a Role)
// grants create/get/list/watch on agentruns in its namespace only.
type AgentRunInvoker struct {
	// Client is an in-pod controller-runtime client (in-cluster config).
	Client client.Client
	// Namespace is the pod's own namespace (downward API); children are created
	// here. Empty disables the invoker (fail-closed).
	Namespace string
	// ParentRun is this run's name, stamped on children for the delegation tree.
	ParentRun string
	// ParentRunUID, when set (operator downward API AGENT_RUN_UID), makes each
	// child OwnerReferenced by this parent run so deleting the parent GCs the
	// whole subtree. Empty = label-only linkage (no GC).
	ParentRunUID string
	// Depth is this run's position in the delegation tree (0 = top level).
	Depth int
	// MaxDepth bounds recursion. <=0 means depth 1 (this run may spawn children,
	// but those children may not) — the conservative default.
	MaxDepth int
	// Poll is the child-status poll interval (default 2s, matching the
	// controller's level-triggered cadence).
	Poll time.Duration
}

// Invoke creates the child AgentRun and blocks until it is terminal.
func (i *AgentRunInvoker) Invoke(ctx context.Context, tool v1.Tool, args json.RawMessage) (rt.Observation, error) {
	if i.Client == nil || i.Namespace == "" {
		return rt.Observation{}, fmt.Errorf("a2a: invoker not configured (no in-pod client/namespace)")
	}
	if tool.Spec.Agent == nil || tool.Spec.Agent.Ref.Name == "" {
		return rt.Observation{}, fmt.Errorf("a2a: tool %q is kind=agent but has no spec.agent.ref.name", tool.Name)
	}
	maxDepth := i.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 1
	}
	if i.Depth >= maxDepth {
		return rt.Observation{}, fmt.Errorf("a2a: recursion depth %d would exceed max %d (refusing to spawn child)", i.Depth, maxDepth)
	}
	target := tool.Spec.Agent.Ref.Name

	// Decode args into a native value so unstructured can hold it (it rejects
	// json.RawMessage). Empty/invalid args become an empty object.
	var input any = map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &input); err != nil {
			return rt.Observation{}, fmt.Errorf("a2a: tool args are not valid JSON: %w", err)
		}
	}
	spec := map[string]any{"agentRef": target, "input": input}
	if mt := tool.Spec.Agent.MaxTokens; mt > 0 {
		spec["budgetOverride"] = map[string]any{"maxTokens": mt}
	}

	child := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	child.SetGroupVersionKind(agentRunGVK)
	child.SetNamespace(i.Namespace)
	child.SetGenerateName(childPrefix(i.ParentRun, target))
	child.SetLabels(map[string]string{
		ParentRunLabel: i.ParentRun,
		DepthLabel:     strconv.Itoa(i.Depth + 1),
	})
	// OwnerReference the child to this parent run (same namespace) so deleting the
	// parent garbage-collects the subtree. GC-only: not a controller ref (the
	// child has its own reconciler).
	if i.ParentRunUID != "" && i.ParentRun != "" {
		child.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: agentRunGVK.GroupVersion().String(),
			Kind:       agentRunGVK.Kind,
			Name:       i.ParentRun,
			UID:        types.UID(i.ParentRunUID),
		}})
	}

	if err := i.Client.Create(ctx, child); err != nil {
		return rt.Observation{}, fmt.Errorf("a2a: create child AgentRun for %q: %w", target, err)
	}
	name := child.GetName()

	poll := i.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	start := timeNow()
	tick := time.NewTimer(0)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// The parent ctx is cancelled (run cancelled / pod SIGTERM). An
			// OwnerReference only GCs the child when the parent OBJECT is deleted;
			// on cancellation the object lives on, so best-effort delete the child
			// here (fresh ctx) to avoid a leaked running child.
			delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = i.Client.Delete(delCtx, child)
			cancel()
			return rt.Observation{}, fmt.Errorf("a2a: waiting on child %s: %w", name, ctx.Err())
		case <-tick.C:
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(agentRunGVK)
		if err := i.Client.Get(ctx, types.NamespacedName{Namespace: i.Namespace, Name: name}, got); err != nil {
			tick.Reset(poll)
			continue
		}
		state, _, _ := unstructured.NestedString(got.Object, "status", "state")
		switch v1.Phase(state) {
		case v1.PhaseCompleted:
			out, _, _ := unstructured.NestedFieldCopy(got.Object, "status", "output")
			raw, err := json.Marshal(out)
			if err != nil {
				raw = []byte("null")
			}
			// Roll the child's usage up into the parent (field-wise): the parent
			// "spent" the child's tokens + tool-calls by delegating to it.
			tokens, _, _ := unstructured.NestedInt64(got.Object, "status", "usage", "tokens")
			calls, _, _ := unstructured.NestedInt64(got.Object, "status", "usage", "toolCalls")
			return rt.Observation{Output: raw, DurationMs: msSince(start), Tokens: tokens, ToolCalls: int32(calls)}, nil
		case v1.PhaseFailed, v1.PhaseCancelled, v1.PhaseExpired:
			reason, _, _ := unstructured.NestedString(got.Object, "status", "reason")
			return rt.Observation{}, fmt.Errorf("a2a: child %s ended %s (%s)", name, state, reason)
		}
		tick.Reset(poll)
	}
}

// childPrefix builds a generateName for a child run; keeps it under the 63-char
// k8s name budget by truncating the parent component.
func childPrefix(parent, target string) string {
	base := parent
	if base == "" {
		base = "a2a"
	}
	if len(base) > 30 {
		base = base[:30]
	}
	if len(target) > 20 {
		target = target[:20]
	}
	return base + "-" + target + "-"
}

func timeNow() time.Time { return time.Now() }
func msSince(t time.Time) int64 {
	return time.Since(t).Milliseconds()
}
