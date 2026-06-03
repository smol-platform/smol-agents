package agentbench

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"time"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// Sample is a single observed run of a case.
type Sample struct {
	// LatencyMs is EndedAt-StartedAt from RunStatus (controller-observed,
	// driver-independent). 0 when either timestamp is missing.
	LatencyMs int64 `json:"latencyMs"`
	// Tokens is status.usage.tokens — REAL only on the Hermes path, 0 for CLI
	// kinds by contract.
	Tokens int64 `json:"tokens"`
	// TokensReal is true when the harness actually reports tokens (Hermes). When
	// false, Tokens is a contractual 0, not a measurement.
	TokensReal bool `json:"tokensReal"`
	// CostUSD is null for CLI kinds (never fabricated); set only when derivable.
	CostUSD *float64 `json:"costUSD"`
	// ColdStartMs is creationTimestamp->startedAt for the first (cold) sample;
	// nil for warm samples.
	ColdStartMs *int64 `json:"coldStartMs,omitempty"`
	// Terminal is the run's terminal phase.
	Terminal pure.Phase `json:"terminal"`
	// Err is set when the sample failed to execute (driver/collect error), as
	// distinct from a non-Succeeded terminal phase.
	Err string `json:"err,omitempty"`
}

// SampleFromStatus derives a Sample from a terminal RunStatus. tokensReal is the
// caller's knowledge of whether the harness kind reports real tokens.
func SampleFromStatus(status pure.RunStatus, tokensReal bool) Sample {
	s := Sample{
		Tokens:     status.Usage.Tokens,
		TokensReal: tokensReal,
		Terminal:   status.State,
	}
	if status.StartedAt != nil && status.EndedAt != nil {
		s.LatencyMs = status.EndedAt.Sub(status.StartedAt.Time).Milliseconds()
	}
	return s
}

// Percentiles holds latency distribution stats (milliseconds).
type Percentiles struct {
	P50  int64 `json:"p50"`
	P95  int64 `json:"p95"`
	P99  int64 `json:"p99"`
	Min  int64 `json:"min"`
	Max  int64 `json:"max"`
	Mean int64 `json:"mean"`
}

// TokenStats aggregates token usage across samples.
type TokenStats struct {
	Total int64 `json:"total"`
	Mean  int64 `json:"mean"`
	Max   int64 `json:"max"`
	// Real reports whether these are real measurements (Hermes) or contractual
	// zeros (CLI kinds).
	Real bool `json:"real"`
}

// Aggregate is the per-case rollup of all samples.
type Aggregate struct {
	Samples      int         `json:"samples"`
	LatencyMs    Percentiles `json:"latencyMs"`
	Tokens       TokenStats  `json:"tokens"`
	CostUSD      *float64    `json:"costUSD"`
	ColdStartMs  *int64      `json:"coldStartMs,omitempty"`
	ErrorRatePct float64     `json:"errorRatePct"`
	// ThroughputRunsPerMin is samples / wallElapsed across the case's samples.
	ThroughputRunsPerMin float64 `json:"throughputRunsPerMin"`
}

// Aggregate rolls up samples into percentiles + token/cost/error stats.
// wallElapsed is the wall time spent running this case's samples (for
// throughput); pass 0 to omit throughput.
func AggregateSamples(samples []Sample, wallElapsed time.Duration) Aggregate {
	agg := Aggregate{Samples: len(samples)}
	if len(samples) == 0 {
		return agg
	}
	lat := make([]int64, 0, len(samples))
	var tokTotal, tokMax int64
	var errCount int
	tokensReal := false
	var costSum float64
	costSeen := false
	for _, s := range samples {
		lat = append(lat, s.LatencyMs)
		tokTotal += s.Tokens
		if s.Tokens > tokMax {
			tokMax = s.Tokens
		}
		if s.TokensReal {
			tokensReal = true
		}
		if s.CostUSD != nil {
			costSum += *s.CostUSD
			costSeen = true
		}
		if s.Err != "" || s.Terminal != pure.PhaseCompleted {
			errCount++
		}
		if s.ColdStartMs != nil && agg.ColdStartMs == nil {
			agg.ColdStartMs = s.ColdStartMs
		}
	}
	agg.LatencyMs = percentiles(lat)
	agg.Tokens = TokenStats{
		Total: tokTotal,
		Mean:  tokTotal / int64(len(samples)),
		Max:   tokMax,
		Real:  tokensReal,
	}
	if costSeen {
		c := costSum
		agg.CostUSD = &c
	}
	agg.ErrorRatePct = round2(float64(errCount) / float64(len(samples)) * 100)
	if wallElapsed > 0 {
		agg.ThroughputRunsPerMin = round2(float64(len(samples)) / wallElapsed.Minutes())
	}
	return agg
}

// percentiles computes p50/p95/p99 + min/max/mean over a latency slice using
// the nearest-rank method.
func percentiles(vals []int64) Percentiles {
	if len(vals) == 0 {
		return Percentiles{}
	}
	sorted := append([]int64(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	return Percentiles{
		P50:  nearestRank(sorted, 50),
		P95:  nearestRank(sorted, 95),
		P99:  nearestRank(sorted, 99),
		Min:  sorted[0],
		Max:  sorted[len(sorted)-1],
		Mean: sum / int64(len(sorted)),
	}
}

// nearestRank returns the pth percentile (1..100) using nearest-rank on a
// pre-sorted ascending slice.
func nearestRank(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(float64(p) / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// GateResult is the evaluation of one metric gate against an aggregate. Want/Got
// are raw JSON scalars (number or boolean).
type GateResult struct {
	Name string          `json:"name"`
	Op   GateOp          `json:"op"`
	Want json.RawMessage `json:"want"`
	Got  json.RawMessage `json:"got"`
	Pass bool            `json:"pass"`
	// NA marks a gate that does not apply (e.g. a token gate on a CLI kind whose
	// tokens are a contractual 0). NA gates do not fail the case.
	NA   bool   `json:"na,omitempty"`
	Note string `json:"note,omitempty"`
}

// EvalGate evaluates one Gate against an aggregate + the oracle verdict and
// returns a GateResult. Token/cost gates on CLI kinds (tokensReal=false) resolve
// to NA (not a failure) per the response-richness contract.
func EvalGate(g Gate, agg Aggregate, verdict VerdictKind) GateResult {
	res := GateResult{Name: g.Metric, Op: g.Op, Want: g.Want}
	if g.Metric == "oracle.pass" {
		got := verdict == VerdictPass || verdict == VerdictBlocked
		res.Got = boolRaw(got)
		res.Pass = compareBool(g.Op, got, g.wantBool())
		return res
	}
	val, ok := metricValue(g.Metric, agg)
	if !ok {
		res.NA = true
		res.Pass = true
		res.Note = "unknown metric"
		return res
	}
	// Token/cost gates are N/A on CLI kinds (contractual zeros).
	if isTokenOrCostMetric(g.Metric) && !agg.Tokens.Real {
		res.NA = true
		res.Pass = true
		res.Got = json.RawMessage("0")
		res.Note = "N/A: tokens/cost are 0 by contract on this (CLI) harness kind"
		return res
	}
	res.Got = json.RawMessage(trimFloat(val))
	want, hasWant := g.wantFloat()
	if !hasWant {
		res.NA = true
		res.Pass = true
		res.Note = "non-numeric want"
		return res
	}
	res.Pass = compareFloat(g.Op, val, want)
	return res
}

func metricValue(metric string, agg Aggregate) (float64, bool) {
	switch metric {
	case "latency.p50.ms":
		return float64(agg.LatencyMs.P50), true
	case "latency.p95.ms":
		return float64(agg.LatencyMs.P95), true
	case "latency.p99.ms":
		return float64(agg.LatencyMs.P99), true
	case "latency.max.ms":
		return float64(agg.LatencyMs.Max), true
	case "tokens.total":
		return float64(agg.Tokens.Total), true
	case "tokens.max":
		return float64(agg.Tokens.Max), true
	case "tokens.mean":
		return float64(agg.Tokens.Mean), true
	case "errorRate.pct":
		return agg.ErrorRatePct, true
	case "throughput.runsPerMin":
		return agg.ThroughputRunsPerMin, true
	case "coldStart.ms":
		if agg.ColdStartMs == nil {
			return 0, false
		}
		return float64(*agg.ColdStartMs), true
	case "cost.usd":
		if agg.CostUSD == nil {
			return 0, false
		}
		return *agg.CostUSD, true
	}
	return 0, false
}

func isTokenOrCostMetric(metric string) bool {
	switch metric {
	case "tokens.total", "tokens.max", "tokens.mean", "cost.usd":
		return true
	}
	return false
}

func compareFloat(op GateOp, got, want float64) bool {
	switch op {
	case GateLTE:
		return got <= want
	case GateGTE:
		return got >= want
	case GateEQ:
		return got == want
	}
	return false
}

func compareBool(op GateOp, got, want bool) bool {
	switch op {
	case GateEQ:
		return got == want
	case GateGTE:
		return boolToF(got) >= boolToF(want)
	case GateLTE:
		return boolToF(got) <= boolToF(want)
	}
	return false
}

func boolRaw(b bool) json.RawMessage {
	if b {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}

func boolToF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func trimFloat(f float64) string {
	if f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
