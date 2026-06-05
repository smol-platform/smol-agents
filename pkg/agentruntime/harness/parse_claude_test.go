package harness

import "testing"

func TestParseClaudeJSON(t *testing.T) {
	env := `{"result":"the answer is 42","session_id":"sess-abc","total_cost_usd":0.0123,"usage":{"input_tokens":100,"output_tokens":40}}`
	out, in, outTok, cost, sid := parseClaudeJSON([]byte(env))
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

	// Malformed envelope → raw bytes + zeros (degrade, never error).
	raw := []byte("not json at all")
	out2, in2, outTok2, cost2, _ := parseClaudeJSON(raw)
	if string(out2) != "not json at all" || in2 != 0 || outTok2 != 0 || cost2 != 0 {
		t.Errorf("malformed must degrade to raw+zeros: %q %d %d %d", out2, in2, outTok2, cost2)
	}
}
