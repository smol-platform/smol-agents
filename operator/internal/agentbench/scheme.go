// Package agentbench is the benchmarking + verification runner for the
// smol-agents platform. It deploys a declared fleet of full-stack agents
// against real LLM backends, submits workloads (via AgentRun CRs or the
// agentgateway turn API), runs correctness oracles + perf metrics against the
// controller-observed pure.RunStatus, and emits results.json + report.md.
//
// Honesty rules (load-bearing, see docs/design/benchmarking-platform.md):
//   - Tokens/cost are REAL only on the Hermes harness path; CLI kinds report 0
//     by contract. The runner records that, it never fabricates a number.
//   - usage.toolCalls is structurally 0 on the harness path — no oracle gates
//     on it. Tool execution is proven by output side-effects.
//   - Loop-mode tool calls are rejected at runtime; the tool_rejected oracle
//     asserts exactly that.
//   - A blocked-tier case carries a NEGATIVE oracle; if it unexpectedly passes
//     (the gap got fixed), the case FAILs (anti-staleness tripwire).
package agentbench

import (
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

// NewScheme builds a runtime.Scheme with the core k8s types plus the
// runtime.agents.smol-agents.ai/v1 agent-model CRDs. It mirrors the operator's
// scheme registration (operator/cmd/manager/main.go) so the oracle code reads
// the exact same typed pure.RunStatus the controller folds.
func NewScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = amv1.AddToScheme(scheme)
	return scheme
}
