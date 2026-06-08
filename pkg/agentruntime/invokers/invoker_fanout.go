package invokers

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	rt "github.com/smol-platform/smol-agents/pkg/agentmodel/runtime"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// FanoutInvoker implements ToolInvoker for ToolKind=fanout (LangGraph Send-style
// runtime map-reduce): one LLM tool call spawns one CHILD AgentRun per item in a
// runtime-computed list, runs them concurrently under a HARD width cap, blocks
// until all are terminal, and folds their outputs via the declared reducer. It is
// the A2A spawn-and-fold (invoker_agent.go) generalized from 1 child to N, and
// reuses the SAME buildChildRun + spawnAndPoll helpers.
//
// Safety (D1/D3/D10): children run in the SAME namespace; total children per call
// are HARD-clamped to MaxWidth (operator FANOUT_MAX_WIDTH — fail-closed if
// unset); concurrency is min(spec.MaxParallel, MaxWidth); EVERY child is a normal
// AgentRun that passes the per-namespace admission queue (no cap bypass); each
// child inherits Depth+1 and is refused past MaxDepth (fork-bomb guard); children
// are OwnerReferenced to the parent for subtree GC. Width gates on COUNT, never
// on cost/toolCalls.
type FanoutInvoker struct {
	Client       client.Client
	Namespace    string
	ParentRun    string
	ParentRunUID string
	Depth        int
	MaxDepth     int
	Poll         time.Duration
	// MaxWidth is the operator's hard ceiling on children per fanout call
	// (FANOUT_MAX_WIDTH). <= 0 disables the invoker (fail-closed).
	MaxWidth int
}

type fanoutArgs struct {
	// Items is the runtime list; each element becomes one child's spec.input.
	Items []json.RawMessage `json:"items"`
}

type childResult struct {
	obs rt.Observation
	err error
}

func (i *FanoutInvoker) Invoke(ctx context.Context, tool v1.Tool, args json.RawMessage) (rt.Observation, error) {
	if i.Client == nil || i.Namespace == "" {
		return rt.Observation{}, fmt.Errorf("fanout: invoker not configured (no in-pod client/namespace)")
	}
	f := tool.Spec.Fanout
	if f == nil || f.Ref.Name == "" {
		return rt.Observation{}, fmt.Errorf("fanout: tool %q is kind=fanout but has no spec.fanout.ref.name", tool.Name)
	}
	if i.MaxWidth <= 0 {
		return rt.Observation{}, fmt.Errorf("fanout: FANOUT_MAX_WIDTH not configured — refusing unbounded fan-out (fail-closed)")
	}
	maxDepth := i.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 1
	}
	if i.Depth >= maxDepth {
		return rt.Observation{}, fmt.Errorf("fanout: recursion depth %d would exceed max %d (refusing to fan out)", i.Depth, maxDepth)
	}

	var fa fanoutArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &fa); err != nil {
			return rt.Observation{}, fmt.Errorf("fanout: args must be {\"items\":[...]}: %w", err)
		}
	}
	if len(fa.Items) == 0 {
		return rt.Observation{}, fmt.Errorf("fanout: no items to fan out over")
	}
	// HARD total clamp: a single call may create at most MaxWidth children — the
	// central blast-radius guard (concurrency is clamped separately below).
	if len(fa.Items) > i.MaxWidth {
		return rt.Observation{}, fmt.Errorf("fanout: %d items exceeds FANOUT_MAX_WIDTH %d (refusing)", len(fa.Items), i.MaxWidth)
	}

	width := int(f.MaxParallel)
	if width <= 0 {
		width = len(fa.Items)
	}
	if width > i.MaxWidth {
		width = i.MaxWidth
	}

	reduce := f.EffectiveReduce()
	results := make([]childResult, len(fa.Items))
	sem := make(chan struct{}, width)
	fanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for idx, item := range fa.Items {
		wg.Add(1)
		go func(idx int, item json.RawMessage) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-fanCtx.Done():
				results[idx] = childResult{err: fanCtx.Err()}
				return
			}
			defer func() { <-sem }()
			var input any = map[string]any{}
			if len(item) > 0 {
				if err := json.Unmarshal(item, &input); err != nil {
					results[idx] = childResult{err: fmt.Errorf("item %d not valid JSON: %w", idx, err)}
					return
				}
			}
			child := buildChildRun(i.Namespace, i.ParentRun, i.ParentRunUID, f.Ref.Name, i.Depth, input, f.PerItemMaxTokens)
			obs, err := spawnAndPoll(fanCtx, i.Client, i.Namespace, child, i.Poll)
			results[idx] = childResult{obs: obs, err: err}
			// first-success: the first child to complete cancels the rest, whose
			// spawnAndPoll then ctx-cancel-deletes their children.
			if err == nil && reduce == v1.FanoutFirstSuccess {
				cancel()
			}
		}(idx, item)
	}
	wg.Wait()
	return reduceFanout(reduce, results)
}

// reduceFanout folds child results: usage is summed FIELD-WISE (never Usage.Add)
// across successful children; outputs are combined per the reducer. A reducer
// surfaces the error count (never silently drops a failure); all-failed is an
// error.
func reduceFanout(mode v1.FanoutReducer, results []childResult) (rt.Observation, error) {
	var tokens int64
	var calls int32
	errCount := 0
	succ := make([]rt.Observation, 0, len(results))
	for _, r := range results {
		if r.err != nil {
			errCount++
			continue
		}
		tokens += r.obs.Tokens
		calls += r.obs.ToolCalls
		succ = append(succ, r.obs)
	}
	if len(succ) == 0 {
		return rt.Observation{}, fmt.Errorf("fanout: all %d children failed", len(results))
	}

	var out any
	switch mode {
	case v1.FanoutFirstSuccess:
		// succ is in item order; the first success is the lowest-index winner.
		return rt.Observation{Output: succ[0].Output, Tokens: tokens, ToolCalls: calls}, nil
	case v1.FanoutMerge:
		merged := map[string]any{}
		for _, o := range succ {
			var m map[string]any
			if json.Unmarshal(o.Output, &m) == nil {
				for k, v := range m {
					merged[k] = v // key-last-wins
				}
			}
		}
		out = map[string]any{"merged": merged, "errors": errCount}
	default: // concat
		arr := make([]json.RawMessage, len(succ))
		for j, o := range succ {
			if len(o.Output) == 0 {
				arr[j] = json.RawMessage("null")
			} else {
				arr[j] = o.Output
			}
		}
		out = map[string]any{"results": arr, "errors": errCount}
	}
	body, err := json.Marshal(out)
	if err != nil {
		return rt.Observation{}, err
	}
	return rt.Observation{Output: body, Tokens: tokens, ToolCalls: calls}, nil
}
