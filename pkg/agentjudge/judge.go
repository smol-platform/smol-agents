// Package agentjudge is a calibrated LLM-as-judge verifier primitive: it grades a
// candidate output against a first-class rubric and returns a STRUCTURED verdict
// (pass + score + feedback), vs brittle exact-match. A judgment is one bounded
// LLM call, so its tokens fold FIELD-WISE into v1.Usage (observability only —
// never a gate). The judge is itself fallible, so Calibrate (calibrate.go) gates
// it on a measured agreement-rate before use.
//
// It backs two seams: the generator-verifier loop's VerifierFunc
// (pkg/turnmodel/team) and an agentbench semantic oracle.
package agentjudge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// Criterion is one weighted rubric dimension.
type Criterion struct {
	Name     string
	Weight   int
	Guidance string
}

// JudgeSpec is what to grade against.
type JudgeSpec struct {
	// Criteria is the overall acceptance standard (required, non-empty).
	Criteria string
	// Rubric optionally decomposes the criteria into weighted dimensions.
	Rubric []Criterion
	// Reference, when non-nil, is the gold answer (reference-based grading);
	// nil grades reference-free against the criteria alone.
	Reference *string
}

// Judgment is the structured verdict.
type Judgment struct {
	Pass         bool
	Score        int // 0-100
	Feedback     string
	PerCriterion map[string]int
	// Usage is the judge call's own token cost, field-wise (obs-only, never gates).
	Usage v1.Usage
	// JudgeModel records which model graded (audit / calibration).
	JudgeModel string
}

// Judge grades candidates with an LLM. Model is the judge model — keep it
// separable from (and often cheaper than) the generator's model. The LLM must be
// the run's OWN per-namespace provider (D1: never a shared cross-tenant judge).
type Judge struct {
	LLM   agentruntime.LLM
	Model v1.ModelRef
}

// ErrNoVerdict is returned when the model produced no parseable final verdict.
var ErrNoVerdict = errors.New("agentjudge: model returned no final verdict")

// Grade evaluates candidate against spec and returns a structured Judgment.
func (j *Judge) Grade(ctx context.Context, spec JudgeSpec, candidate string) (Judgment, error) {
	if j.LLM == nil {
		return Judgment{}, errors.New("agentjudge: no LLM configured")
	}
	if spec.Criteria == "" {
		return Judgment{}, errors.New("agentjudge: spec.Criteria is required")
	}
	in := map[string]any{"candidate": candidate}
	if spec.Reference != nil {
		in["reference"] = *spec.Reference
	}
	inRaw, err := json.Marshal(in)
	if err != nil {
		return Judgment{}, err
	}
	dec, err := j.LLM.Chat(ctx, agentruntime.ChatRequest{
		Model:        j.Model,
		Instructions: buildPrompt(spec),
		Input:        inRaw,
	})
	if err != nil {
		return Judgment{}, fmt.Errorf("agentjudge: judge call: %w", err)
	}
	if dec.FinalAnswer == nil {
		return Judgment{}, ErrNoVerdict
	}
	var v struct {
		Score        int            `json:"score"`
		Pass         bool           `json:"pass"`
		Comment      string         `json:"comment"`
		PerCriterion map[string]int `json:"perCriterion"`
	}
	if err := json.Unmarshal(dec.FinalAnswer.Output, &v); err != nil {
		return Judgment{}, fmt.Errorf("agentjudge: verdict not valid JSON: %w", err)
	}
	if v.Score < 0 {
		v.Score = 0
	}
	if v.Score > 100 {
		v.Score = 100
	}
	return Judgment{
		Pass:         v.Pass,
		Score:        v.Score,
		Feedback:     v.Comment,
		PerCriterion: v.PerCriterion,
		// Judge tokens fold field-wise — observability only, NEVER a gate, and
		// toolCalls untouched (a judgment makes no tool calls).
		Usage:      v1.Usage{Tokens: dec.TokensIn + dec.TokensOut},
		JudgeModel: j.Model.Name,
	}, nil
}

// buildPrompt renders the judge system prompt: the criteria + rubric, a
// verbosity/position-neutral instruction (bias control), and the required JSON
// verdict shape (the model must answer with ONLY this object).
func buildPrompt(spec JudgeSpec) string {
	var b strings.Builder
	b.WriteString("You are a strict, impartial evaluator. Grade the candidate output in the input ")
	b.WriteString("against the criteria below. Judge ONLY on substance — never reward length, ")
	b.WriteString("verbosity, or position. Be calibrated: pass only if the criteria are genuinely met.\n\n")
	b.WriteString("CRITERIA: " + spec.Criteria + "\n")
	if len(spec.Rubric) > 0 {
		b.WriteString("RUBRIC (weighted):\n")
		for _, c := range spec.Rubric {
			fmt.Fprintf(&b, "- %s (weight %d): %s\n", c.Name, c.Weight, c.Guidance)
		}
	}
	if spec.Reference != nil {
		b.WriteString("A reference (gold) answer is provided in input.reference; grade the candidate against it.\n")
	} else {
		b.WriteString("No reference is provided; grade the candidate against the criteria directly.\n")
	}
	b.WriteString("\nRespond with ONLY a JSON object: ")
	b.WriteString(`{"score": <0-100>, "pass": <bool>, "comment": "<concise reason>"`)
	if len(spec.Rubric) > 0 {
		b.WriteString(`, "perCriterion": {"<name>": <0-100>, ...}`)
	}
	b.WriteString("}")
	return b.String()
}
