package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/smol-platform/smol-agents/pkg/agentjudge"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/agentruntime/coordinator"
	"github.com/smol-platform/smol-agents/pkg/turnmodel/team"
)

// agentTeamGVK is the AgentTeam CRD identity; the coordinator reads its team via
// unstructured so the in-pod runtime needs no dependency on the operator API.
var agentTeamGVK = schema.GroupVersionKind{
	Group:   "runtime.agents.smol-agents.ai",
	Version: "v1",
	Kind:    "AgentTeam",
}

// maybeRunCoordinator runs the generator-verifier convergence loop when this run
// is a team coordinator (rv3.1 S5). Detection: the lead coordinator carries team
// context (TEAM_NAME) but no member identity (TEAM_MEMBER) — members always get
// TEAM_MEMBER. handled=false falls through to the normal RunOnce loop for any
// other run: a non-team run, a member run, a non-generator-verifier team, or no
// in-cluster access. Once a generator-verifier coordinator is confirmed, every
// failure is the coordinator's terminal result (handled=true, err set).
func maybeRunCoordinator(ctx context.Context, dir string, toolInvokers map[v1.ToolKind]agentruntime.ToolInvoker, llm agentruntime.LLM) (agentruntime.RunResult, bool, error) {
	teamName := os.Getenv("TEAM_NAME")
	if teamName == "" || os.Getenv("TEAM_MEMBER") != "" {
		return agentruntime.RunResult{}, false, nil
	}
	ns := os.Getenv("TEAM_NAMESPACE")
	if ns == "" {
		ns = os.Getenv("POD_NAMESPACE")
	}
	if ns == "" {
		return agentruntime.RunResult{}, false, nil
	}

	// Read the AgentTeam in-pod (unstructured — no operator API dependency). Any
	// access failure falls through to the plain loop rather than failing the run:
	// without apiserver authority this run simply isn't coordinator-capable.
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return agentruntime.RunResult{}, false, nil
	}
	kc, err := crclient.New(cfg, crclient.Options{})
	if err != nil {
		return agentruntime.RunResult{}, false, nil
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(agentTeamGVK)
	if err := kc.Get(ctx, types.NamespacedName{Namespace: ns, Name: teamName}, obj); err != nil {
		return agentruntime.RunResult{}, false, nil
	}
	specMap, _, _ := unstructured.NestedMap(obj.Object, "spec")
	specJSON, err := json.Marshal(specMap)
	if err != nil {
		return agentruntime.RunResult{}, false, nil
	}
	spec, err := coordinator.DecodeAgentTeamSpec(specJSON)
	if err != nil {
		return agentruntime.RunResult{}, false, nil
	}
	at := v1.AgentTeam{Name: teamName, Spec: spec}

	// Only the generator-verifier pattern is coordinator-driven; the other
	// patterns are served by the lead's own plan-act-observe loop (RunOnce).
	if at.Spec.EffectivePattern() != v1.TeamPatternGeneratorVerifier {
		return agentruntime.RunResult{}, false, nil
	}

	// Confirmed coordinator: from here a failure is this run's terminal result.
	if llm == nil {
		return agentruntime.RunResult{}, true, fmt.Errorf("coordinator: no loop LLM for the judge verifier (provider missing?)")
	}
	invoker := toolInvokers[v1.ToolAgent]
	if invoker == nil {
		return agentruntime.RunResult{}, true, fmt.Errorf("coordinator: kind=agent (A2A) invoker not wired — no in-cluster delegation authority")
	}
	objective, model, err := coordinatorInputs(dir)
	if err != nil {
		return agentruntime.RunResult{}, true, err
	}
	verify := coordinator.NewJudgeVerifier(llm, model, agentjudge.JudgeSpec{})

	res, err := coordinator.Run(ctx, at, objective, invoker, verify)
	if err != nil {
		return agentruntime.RunResult{}, true, err
	}
	return foldCoordinatorResult(res), true, nil
}

// coordinatorInputs reads the coordinator's objective (the run input) and judge
// model (the lead Agent's model) from the mounted run/agent specs.
func coordinatorInputs(dir string) (objective string, model v1.ModelRef, err error) {
	var run v1.AgentRunSpec
	if e := readJSONFileLocal(filepath.Join(dir, agentruntime.RunSpecFile), &run); e != nil {
		return "", v1.ModelRef{}, fmt.Errorf("coordinator: load run spec: %w", e)
	}
	var agent v1.Agent
	if e := readJSONFileLocal(filepath.Join(dir, agentruntime.AgentSpecFile), &agent); e != nil {
		return "", v1.ModelRef{}, fmt.Errorf("coordinator: load agent spec: %w", e)
	}
	return asText(run.Input), agent.Spec.Model, nil
}

// coordinatorOutput is the coordinator run's folded output — the converged result
// plus the loop's verdict/accounting, so the AgentTeam status (and any caller)
// can see what the team produced and why it stopped.
type coordinatorOutput struct {
	Accepted   bool   `json:"accepted"`
	StopReason string `json:"stopReason"`
	Rounds     int    `json:"rounds"`
	Result     string `json:"result"`
	Score      int    `json:"score,omitempty"`
	Feedback   string `json:"feedback,omitempty"`
	HookReason string `json:"hookReason,omitempty"`
}

// foldCoordinatorResult maps a CoordinatorResult into the RunResult the controller
// folds. The run COMPLETED whenever the loop ran cleanly — non-acceptance
// (max-iterations / budget / a quality veto) is a legitimate outcome carried in
// the output, not a run failure (a genuine error is the err path above). Usage
// rolls up field-wise from the loop.
func foldCoordinatorResult(res team.CoordinatorResult) agentruntime.RunResult {
	out, err := json.Marshal(coordinatorOutput{
		Accepted:   res.Accepted,
		StopReason: res.StopReason,
		Rounds:     res.Convergence.Rounds,
		Result:     res.Convergence.Best.Content,
		Score:      res.Convergence.Verdict.Score,
		Feedback:   res.Convergence.Verdict.Feedback,
		HookReason: res.HookReason,
	})
	if err != nil {
		out = json.RawMessage(`null`)
	}
	return agentruntime.RunResult{
		Phase:             v1.PhaseCompleted,
		Output:            out,
		Usage:             res.Convergence.Usage,
		TerminationReason: res.StopReason,
	}
}

// readJSONFileLocal mirrors the runtime's unexported spec loader (read + decode).
func readJSONFileLocal(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// asText renders a run input as the coordinator's objective: a JSON string is
// unwrapped to its value, any other JSON is passed through verbatim.
func asText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}
