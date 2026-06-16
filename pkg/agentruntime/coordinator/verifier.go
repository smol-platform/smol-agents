package coordinator

import (
	"github.com/smol-platform/smol-agents/pkg/agentjudge"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/turnmodel/team"
)

// NewJudgeVerifier builds the convergence loop's verifier from the coordinator's
// OWN loop LLM (D1: the run's per-namespace provider, never a shared cross-tenant
// judge). It wraps a calibrated agentjudge.Judge with team.JudgeVerifier — the
// verifier half of the coordinator loop (the generator half is A2ADispatcher).
//
// The loop's ConvergenceSpec.Criteria is authoritative and overrides per call
// (JudgeVerifier sets it), so base carries only optional calibration (rubric /
// reference); model selects a grading model, kept separable from — and often
// cheaper than — the generator's.
func NewJudgeVerifier(llm agentruntime.LLM, model v1.ModelRef, base agentjudge.JudgeSpec) team.VerifierFunc {
	j := &agentjudge.Judge{LLM: llm, Model: model}
	return team.JudgeVerifier(j, base)
}
