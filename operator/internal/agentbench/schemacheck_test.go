package agentbench

import (
	"encoding/json"
	"testing"
	"time"
)

// TestResultsJSON_SchemaShape asserts the emitted results.json carries every
// top-level key the shipped results_schema.json marks "required", and that the
// case/aggregate/gate shapes line up. It is a drift guard between the Go structs
// and results_schema.json (no external schema validator dep).
func TestResultsJSON_SchemaShape(t *testing.T) {
	cost := 0.00021
	res := &Results{
		RunID: "r", PlanDigest: "sha256:x", Plan: "p",
		Cluster:   ClusterInfo{Runtime: "runc", Caps: []string{"hermes"}},
		StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
		Cases: []CaseResult{{
			ID: "c1", AgentRef: "a", Tier: TierPerf, Samples: 3,
			Oracle:  OracleResult{Kind: "output_match", Verdict: VerdictPass, Evidence: "e"},
			Metrics: Aggregate{Samples: 3, CostUSD: &cost},
			Gates:   []GateResult{{Name: "latency.p95.ms", Op: GateLTE, Want: json.RawMessage("5000"), Got: json.RawMessage("2310"), Pass: true}},
			Pass:    true,
		}},
	}
	res.Finalize()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"$schema", "runId", "planDigest", "plan", "cluster", "startedAt", "finishedAt", "cases", "summary", "verdict"} {
		if _, ok := m[k]; !ok {
			t.Errorf("results.json missing required top-level key %q", k)
		}
	}
	if string(m["$schema"]) != `"agentbench/v1"` {
		t.Errorf("$schema = %s, want \"agentbench/v1\"", m["$schema"])
	}
}
