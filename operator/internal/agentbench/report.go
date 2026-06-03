package agentbench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// SchemaID is the $schema value stamped on results.json.
const SchemaID = "agentbench/v1"

// ClusterInfo describes the target cluster in the result header.
type ClusterInfo struct {
	Name    string   `json:"name,omitempty"`
	Runtime string   `json:"runtime"`
	Caps    []string `json:"caps"`
	Node    string   `json:"node,omitempty"`
}

// BackendInfo describes the LLM backend in the result header.
type BackendInfo struct {
	Kind     string `json:"kind,omitempty"`
	Model    string `json:"model,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// CaseResult is the per-case rollup written to results.json.
type CaseResult struct {
	ID          string       `json:"id"`
	AgentRef    string       `json:"agentRef"`
	Tier        Tier         `json:"tier"`
	HarnessKind string       `json:"harnessKind,omitempty"`
	Samples     int          `json:"samples"`
	Seed        int64        `json:"seed,omitempty"`
	Nonce       string       `json:"nonce,omitempty"`
	Oracle      OracleResult `json:"oracle"`
	Blocked     *BlockedSpec `json:"blocked,omitempty"`
	Metrics     Aggregate    `json:"metrics"`
	Gates       []GateResult `json:"gates"`
	Pass        bool         `json:"pass"`
	Skipped     bool         `json:"skipped,omitempty"`
	SkipReason  string       `json:"skipReason,omitempty"`
	// Statuses optionally retains each sample's full RunStatus (--record).
	Statuses []pure.RunStatus `json:"statuses,omitempty"`

	// repOutput is the representative sample's RunStatus.Output, threaded into
	// the next fs_roundtrip case in plan order. Not serialized.
	repOutput []byte `json:"-"`
}

// RepOutput returns the representative sample's output (for fs_roundtrip
// threading); always populated regardless of --record.
func (c CaseResult) RepOutput() []byte { return c.repOutput }

// OracleResult is the oracle verdict + evidence as written to results.json.
type OracleResult struct {
	Kind     string      `json:"kind"`
	Verdict  VerdictKind `json:"verdict"`
	Evidence string      `json:"evidence"`
}

// Summary is the run-level tally.
type Summary struct {
	Total        int      `json:"total"`
	Passed       int      `json:"passed"`
	Failed       int      `json:"failed"`
	Skipped      int      `json:"skipped"`
	Blocked      int      `json:"blocked"`
	GateFailures []string `json:"gateFailures"`
}

// Results is the top-level results.json document.
type Results struct {
	Schema     string       `json:"$schema"`
	RunID      string       `json:"runId"`
	PlanDigest string       `json:"planDigest"`
	Plan       string       `json:"plan"`
	Tier       Tier         `json:"tier,omitempty"`
	Cluster    ClusterInfo  `json:"cluster"`
	Backend    BackendInfo  `json:"backend,omitempty"`
	StartedAt  time.Time    `json:"startedAt"`
	FinishedAt time.Time    `json:"finishedAt"`
	Cases      []CaseResult `json:"cases"`
	Summary    Summary      `json:"summary"`
	Verdict    string       `json:"verdict"` // PASS | FAIL
}

// ComputeCasePass derives a case's pass from its oracle verdict + metric gates,
// per §2.5: pass = (verdict ∈ {pass, blocked}) AND all (non-NA) gates pass. A
// skipped case is neither pass nor fail. A blocked case that PASSES its negative
// oracle stays blocked-expected; if its negative oracle FAILs, the case fails
// (anti-staleness).
func ComputeCasePass(verdict VerdictKind, gates []GateResult, skipped bool) bool {
	if skipped {
		return false
	}
	if verdict != VerdictPass && verdict != VerdictBlocked {
		return false
	}
	for _, g := range gates {
		if !g.NA && !g.Pass {
			return false
		}
	}
	return true
}

// Finalize fills Schema/Summary/Verdict from the case results.
func (r *Results) Finalize() {
	r.Schema = SchemaID
	sum := Summary{Total: len(r.Cases)}
	for _, c := range r.Cases {
		switch {
		case c.Skipped:
			sum.Skipped++
		case c.Pass && c.Oracle.Verdict == VerdictBlocked:
			sum.Blocked++
			sum.Passed++
		case c.Pass:
			sum.Passed++
		default:
			sum.Failed++
		}
		for _, g := range c.Gates {
			if !g.NA && !g.Pass {
				sum.GateFailures = append(sum.GateFailures, c.ID+":"+g.Name)
			}
		}
	}
	sort.Strings(sum.GateFailures)
	r.Summary = sum
	if sum.Failed > 0 {
		r.Verdict = "FAIL"
	} else {
		r.Verdict = "PASS"
	}
}

// WriteJSON writes results.json into dir.
func (r *Results) WriteJSON(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "results.json")
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// WriteMarkdown renders report.md (per-tier table + BLOCKED + SKIPPED
// appendices) into dir, in the docs/design house style.
func (r *Results) WriteMarkdown(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "report.md")
	var b strings.Builder

	fmt.Fprintf(&b, "# agentbench report — `%s`\n\n", r.Plan)
	fmt.Fprintf(&b, "> Run `%s` · plan digest `%s` · verdict **%s**\n\n", r.RunID, r.PlanDigest, r.Verdict)
	fmt.Fprintf(&b, "- **Cluster:** %s (runtime `%s`%s) · caps: `%s`\n",
		orDash(r.Cluster.Name), r.Cluster.Runtime, nodeSuffix(r.Cluster.Node), strings.Join(r.Cluster.Caps, ","))
	if r.Backend.Kind != "" {
		fmt.Fprintf(&b, "- **Backend:** %s `%s` @ `%s`\n", r.Backend.Kind, orDash(r.Backend.Model), orDash(r.Backend.Endpoint))
	}
	fmt.Fprintf(&b, "- **Window:** %s → %s\n", r.StartedAt.Format(time.RFC3339), r.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Summary:** total %d · passed %d · failed %d · skipped %d · blocked %d\n\n",
		r.Summary.Total, r.Summary.Passed, r.Summary.Failed, r.Summary.Skipped, r.Summary.Blocked)

	// Per-tier tables.
	tiers := []Tier{TierCorrectness, TierPerf, TierScale, TierIsolation, TierFuture}
	for _, t := range tiers {
		rows := casesInTier(r.Cases, t, false)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "## Tier: %s\n\n", t)
		b.WriteString("| ID | harness | oracle | verdict | p50 ms | p95 ms | tokens | cost | gate |\n")
		b.WriteString("|---|---|---|---|---:|---:|---:|---:|:--:|\n")
		for _, c := range rows {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %d | %s | %s | %s |\n",
				c.ID, orDash(c.HarnessKind), c.Oracle.Kind, verdictMark(c),
				c.Metrics.LatencyMs.P50, c.Metrics.LatencyMs.P95,
				tokenCell(c.Metrics.Tokens), costCell(c.Metrics.CostUSD), gateMark(c.Gates))
		}
		b.WriteString("\n")
	}

	// BLOCKED appendix.
	blocked := filterBlocked(r.Cases)
	if len(blocked) > 0 {
		b.WriteString("## BLOCKED (negative-oracle tripwires)\n\n")
		b.WriteString("| ID | reason | unblock spec | held? |\n|---|---|---|:--:|\n")
		for _, c := range blocked {
			held := "no — STALE, flip to positive"
			if c.Pass {
				held = "yes"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.ID, orDash(c.Blocked.Reason), orDash(c.Blocked.UnblockSpec), held)
		}
		b.WriteString("\n")
	}

	// SKIPPED appendix.
	skipped := filterSkipped(r.Cases)
	if len(skipped) > 0 {
		b.WriteString("## SKIPPED (capability missing)\n\n")
		b.WriteString("| ID | reason |\n|---|---|\n")
		for _, c := range skipped {
			fmt.Fprintf(&b, "| %s | %s |\n", c.ID, orDash(c.SkipReason))
		}
		b.WriteString("\n")
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func casesInTier(cases []CaseResult, t Tier, _ bool) []CaseResult {
	var out []CaseResult
	for _, c := range cases {
		if c.Tier == t {
			out = append(out, c)
		}
	}
	return out
}

func filterBlocked(cases []CaseResult) []CaseResult {
	var out []CaseResult
	for _, c := range cases {
		if c.Blocked != nil {
			out = append(out, c)
		}
	}
	return out
}

func filterSkipped(cases []CaseResult) []CaseResult {
	var out []CaseResult
	for _, c := range cases {
		if c.Skipped {
			out = append(out, c)
		}
	}
	return out
}

func verdictMark(c CaseResult) string {
	if c.Skipped {
		return "SKIP"
	}
	switch c.Oracle.Verdict {
	case VerdictPass:
		if c.Pass {
			return "PASS"
		}
		return "FAIL (gate)"
	case VerdictBlocked:
		if c.Pass {
			return "BLOCKED"
		}
		return "FAIL (stale)"
	case VerdictFail:
		return "FAIL"
	case VerdictSkip:
		return "SKIP"
	}
	return string(c.Oracle.Verdict)
}

func gateMark(gates []GateResult) string {
	for _, g := range gates {
		if !g.NA && !g.Pass {
			return "✗"
		}
	}
	return "✓"
}

func tokenCell(t TokenStats) string {
	if !t.Real {
		return "0 (N/A)"
	}
	return fmt.Sprintf("%d", t.Total)
}

func costCell(c *float64) string {
	if c == nil {
		return "null"
	}
	return fmt.Sprintf("$%.5f", *c)
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func nodeSuffix(node string) string {
	if node == "" {
		return ""
	}
	return ", node " + node
}
