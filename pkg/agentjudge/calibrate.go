package agentjudge

import "context"

// CalibrationCase is a golden (candidate, expected pass/fail) pair used to
// measure a judge's accuracy before trusting it.
type CalibrationCase struct {
	Candidate    string
	ExpectedPass bool
	// Reference, when set, overrides the spec Reference for this case.
	Reference *string
}

// CalibrationReport is the outcome of Calibrate.
type CalibrationReport struct {
	Total         int
	Agreements    int
	AgreementRate float64 // Agreements / Total (0 when Total == 0)
}

// Meets reports whether the measured agreement-rate clears a threshold — the gate
// a caller applies before using the judge (a fallible LLM is trusted only once
// measured).
func (r CalibrationReport) Meets(threshold float64) bool {
	return r.Total > 0 && r.AgreementRate >= threshold
}

// Calibrate grades each golden case and reports how often the judge's pass/fail
// matched the expectation. A judge below the desired agreement-rate should not be
// used (confronting the judge's own fallibility, the brief's bar).
func (j *Judge) Calibrate(ctx context.Context, spec JudgeSpec, cases []CalibrationCase) (CalibrationReport, error) {
	rep := CalibrationReport{Total: len(cases)}
	for _, c := range cases {
		s := spec
		if c.Reference != nil {
			s.Reference = c.Reference
		}
		jd, err := j.Grade(ctx, s, c.Candidate)
		if err != nil {
			return rep, err
		}
		if jd.Pass == c.ExpectedPass {
			rep.Agreements++
		}
	}
	if rep.Total > 0 {
		rep.AgreementRate = float64(rep.Agreements) / float64(rep.Total)
	}
	return rep, nil
}
