package harness

import "testing"

func TestParseClaudeJSON(t *testing.T) {
	env := `{"result":"the answer is 42","session_id":"sess-abc","total_cost_usd":0.0123,"usage":{"input_tokens":100,"output_tokens":40}}`
	out, in, outTok, cost, sid, isErr := parseClaudeJSON([]byte(env))
	if string(out) != "the answer is 42" {
		t.Errorf("output = %q", out)
	}
	if in != 100 || outTok != 40 {
		t.Errorf("tokens = %d/%d, want 100/40", in, outTok)
	}
	if cost != 12 { // round(0.0123 * 1000) = 12
		t.Errorf("costMilli = %d, want 12", cost)
	}
	if sid != "sess-abc" {
		t.Errorf("sessionID = %q", sid)
	}
	if isErr {
		t.Error("is_error absent → isError must be false")
	}

	// is_error:true must be surfaced (claude exits 0 even on a turn error).
	errEnv := `{"result":"tool failed","is_error":true,"usage":{"input_tokens":5,"output_tokens":2}}`
	if _, _, _, _, _, gotErr := parseClaudeJSON([]byte(errEnv)); !gotErr {
		t.Error("is_error:true must parse to isError=true (M3.16)")
	}

	// Malformed envelope → raw bytes + zeros (degrade, never error).
	raw := []byte("not json at all")
	out2, in2, outTok2, cost2, _, isErr2 := parseClaudeJSON(raw)
	if string(out2) != "not json at all" || in2 != 0 || outTok2 != 0 || cost2 != 0 || isErr2 {
		t.Errorf("malformed must degrade to raw+zeros+no-error: %q %d %d %d %v", out2, in2, outTok2, cost2, isErr2)
	}
}
