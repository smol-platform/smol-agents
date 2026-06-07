package team

import (
	"context"

	"github.com/smol-platform/smol-agents/pkg/agentjudge"
)

// JudgeVerifier adapts a calibrated agentjudge.Judge into a VerifierFunc for the
// generator-verifier convergence loop — giving the loop its first real verifier
// (the existing VerifierFunc seam had no production impl). The loop's criteria
// (from ConvergenceSpec.Criteria) is authoritative, so it overrides the base
// spec's Criteria; pass→Accepted, score→Score, comment→Feedback, and the judge's
// own tokens fold field-wise into the attempt usage (obs-only, never a gate).
func JudgeVerifier(j *agentjudge.Judge, base agentjudge.JudgeSpec) VerifierFunc {
	return func(ctx context.Context, a Attempt, criteria string) (Verdict, error) {
		spec := base
		spec.Criteria = criteria
		jd, err := j.Grade(ctx, spec, a.Content)
		if err != nil {
			return Verdict{}, err
		}
		return Verdict{
			Accepted: jd.Pass,
			Score:    jd.Score,
			Feedback: jd.Feedback,
			Usage:    jd.Usage,
		}, nil
	}
}
