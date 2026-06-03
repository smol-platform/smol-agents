package agentbench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// RunOptions configures a single bench run.
type RunOptions struct {
	Tier         Tier
	Driver       DriverKind // override the per-case driver; "" honors the case
	Concurrency  int        // max in-flight samples (>=1)
	Repeat       int        // multiply each case's samples by this (>=1)
	AllowBlocked bool
	Record       bool // retain full RunStatus per sample in results
}

// caseRunner submits + collects a case's samples and evaluates its oracle.
type caseRunner struct {
	driver      Driver
	oracle      OracleImpl
	caps        CapsResult
	harnessKind string
	concurrency int
}

// RunCase executes one case (all samples), runs its oracle + gates, and returns
// a CaseResult. ns is the run namespace; nonce is the fresh per-run nonce (empty
// when the case did not request one). prior is the threaded prior-run output for
// fs_roundtrip.
func (cr *caseRunner) RunCase(ctx context.Context, c BenchCase, ns, nonce string, prior []byte, record bool) CaseResult {
	res := CaseResult{
		ID:          c.Metadata.Name,
		AgentRef:    c.AgentRef,
		Tier:        c.Tier,
		HarnessKind: cr.harnessKind,
		Seed:        c.Seed,
		Nonce:       nonce,
		Blocked:     c.Blocked,
		Oracle:      OracleResult{Kind: c.Oracle.Kind},
	}

	// Capability gate: SKIP loudly if a required cap is missing.
	if missing, ok := cr.caps.HasAll(c.RequiredCaps); !ok {
		res.Skipped = true
		res.SkipReason = "missing capabilities: " + strings.Join(missing, ", ")
		res.Oracle.Verdict = VerdictSkip
		res.Oracle.Evidence = res.SkipReason
		return res
	}

	samples := c.EffectiveSamples()
	res.Samples = samples
	prompt := substituteNonce(c.Input.Prompt, nonce)

	type outcome struct {
		idx    int
		status pure.RunStatus
		sample Sample
		err    error
	}
	results := make([]outcome, samples)
	sem := make(chan struct{}, max1(cr.concurrency))
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < samples; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			h, err := cr.driver.Submit(ctx, c, ns, prompt, i)
			if err != nil {
				results[i] = outcome{idx: i, err: err}
				return
			}
			status, cerr := cr.driver.Collect(ctx, h)
			s := SampleFromStatus(status, tokensReal(cr.harnessKind))
			if cerr != nil {
				s.Err = cerr.Error()
			}
			results[i] = outcome{idx: i, status: status, sample: s, err: cerr}
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)

	// Gather samples + pick a representative status for the oracle (the first
	// successful terminal one, else the first).
	allSamples := make([]Sample, 0, samples)
	var repStatus pure.RunStatus
	repChosen := false
	var firstErr error
	for _, o := range results {
		allSamples = append(allSamples, o.sample)
		if record {
			res.Statuses = append(res.Statuses, o.status)
		}
		if o.err != nil && firstErr == nil {
			firstErr = o.err
		}
		if !repChosen && o.err == nil {
			repStatus = o.status
			repChosen = true
		}
	}
	if !repChosen && len(results) > 0 {
		repStatus = results[0].status
	}
	res.repOutput = repStatus.Output

	res.Metrics = AggregateSamples(allSamples, wall)

	// Run the oracle against the representative status.
	cctx := CollectCtx{
		Case:          c,
		Nonce:         nonce,
		HarnessKind:   cr.harnessKind,
		NodeKernel:    cr.caps.NodeKernel,
		KataAvailable: cr.caps.Has(CapKataFC),
		PriorOutput:   prior,
		SecretValues:  collectSecretValues(c, nonce),
	}
	verdict := cr.oracle.Check(repStatus, cctx)
	res.Oracle.Verdict = verdict.Kind
	res.Oracle.Evidence = verdict.Evidence
	// Surface a driver error in the evidence if the oracle didn't already fail
	// for a substantive reason.
	if firstErr != nil && verdict.Kind == VerdictFail {
		res.Oracle.Evidence += " [driver error: " + firstErr.Error() + "]"
	}

	// Evaluate gates.
	for _, g := range c.Gates {
		res.Gates = append(res.Gates, EvalGate(g, res.Metrics, verdict.Kind))
	}

	res.Pass = ComputeCasePass(verdict.Kind, res.Gates, res.Skipped)
	return res
}

// collectSecretValues pulls the values an oracle should grep for. Today the
// plan carries them via oracle.want (a literal secret or marker); the nonce is
// always included so a fresh-nonce secret_absent has something to assert.
func collectSecretValues(c BenchCase, nonce string) map[string]string {
	if c.Oracle.Kind != "secret_absent" && c.Oracle.Kind != "secret_reach" {
		return nil
	}
	vals := map[string]string{}
	if w := substituteNonce(c.Oracle.Want, nonce); w != "" {
		vals["want"] = w
	}
	if nonce != "" {
		vals["nonce"] = nonce
	}
	return vals
}

// NewNonce returns a fresh 128-bit hex nonce.
func NewNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tokensReal reports whether a harness kind produces real token accounting
// (Hermes only). CLI kinds report 0 by contract.
func tokensReal(harnessKind string) bool {
	return harnessKind == "hermes"
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// SortCasesStable returns cases in a deterministic order (by name) so a run is
// reproducible regardless of map iteration.
func SortCasesStable(cases []BenchCase) []BenchCase {
	out := append([]BenchCase(nil), cases...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

// NewCaseRunner builds a caseRunner for a case, resolving its oracle impl.
func NewCaseRunner(driver Driver, caps CapsResult, harnessKind string, concurrency int, oracleKind string) (*caseRunner, error) {
	o, ok := LookupOracle(oracleKind)
	if !ok {
		return nil, fmt.Errorf("runner: no oracle registered for kind %q", oracleKind)
	}
	return &caseRunner{
		driver:      driver,
		oracle:      o,
		caps:        caps,
		harnessKind: harnessKind,
		concurrency: concurrency,
	}, nil
}
