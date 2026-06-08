package agentjudge

import (
	"context"
	"encoding/json"

	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// AsExecutorVerifier adapts a calibrated Judge into the executor's Verifier seam
// (iru.7) — the single-agent online guardrail. The loop's VerifyCriteria
// overrides the base spec's Criteria; the judge's own tokens fold field-wise into
// the result (obs-only). One judge abstraction now backs both the team
// generator-verifier loop (team.JudgeVerifier) and the single-agent loop.
func AsExecutorVerifier(j *Judge, base JudgeSpec) agentruntime.Verifier {
	return &executorVerifier{j: j, base: base}
}

type executorVerifier struct {
	j    *Judge
	base JudgeSpec
}

func (v *executorVerifier) Verify(ctx context.Context, output json.RawMessage, criteria string) (agentruntime.VerifyResult, error) {
	spec := v.base
	spec.Criteria = criteria
	jd, err := v.j.Grade(ctx, spec, string(output))
	if err != nil {
		return agentruntime.VerifyResult{}, err
	}
	return agentruntime.VerifyResult{
		Accepted: jd.Pass,
		Score:    jd.Score,
		Feedback: jd.Feedback,
		Usage:    jd.Usage,
	}, nil
}
