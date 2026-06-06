package harness

import "testing"

func TestParseCodexJSONL(t *testing.T) {
	// A representative `codex exec --json` stream: a command execution, the final
	// agent message, then turn.completed with usage.
	jsonl := `{"type":"thread.started","thread_id":"t1"}
{"type":"item.completed","item":{"type":"command_execution","command":"ls -la","exit_code":0,"aggregated_output":"file.txt"}}
{"type":"item.completed","item":{"type":"agent_message","text":"the answer is 42"}}
{"type":"turn.completed","usage":{"input_tokens":123,"output_tokens":45}}
`
	out, in, outTok, cost, calls := parseCodexJSONL([]byte(jsonl))
	if string(out) != "the answer is 42" {
		t.Errorf("output = %q, want the agent_message text", out)
	}
	if in != 123 || outTok != 45 {
		t.Errorf("tokens = %d/%d, want 123/45", in, outTok)
	}
	if cost != 0 {
		t.Errorf("cost = %d, want 0 (no cost emitted)", cost)
	}
	if len(calls) != 1 || calls[0].Tool != "exec" {
		t.Fatalf("toolCalls = %+v, want one exec record", calls)
	}
	if string(calls[0].Arguments) != `{"command":"ls -la"}` {
		t.Errorf("exec args = %s", calls[0].Arguments)
	}
}

// A truncated final line (process killed mid-write) must not break the parse —
// the complete earlier lines still yield usage + output.
func TestParseCodexJSONL_PartialLastLine(t *testing.T) {
	jsonl := `{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}
{"type":"turn.compl` // truncated
	out, in, outTok, _, _ := parseCodexJSONL([]byte(jsonl))
	if string(out) != "done" || in != 10 || outTok != 2 {
		t.Errorf("partial-tail parse = %q %d/%d, want done 10/2", out, in, outTok)
	}
}

// A non-JSONL blob (codex run without --json, or a crash) degrades to opaque
// output with zero usage — never a panic.
func TestParseCodexJSONL_MalformedRawFallback(t *testing.T) {
	raw := []byte("plain codex output, not json\n")
	out, in, outTok, cost, calls := parseCodexJSONL(raw)
	if string(out) != string(raw) || in != 0 || outTok != 0 || cost != 0 || calls != nil {
		t.Errorf("malformed → raw+zeros expected, got %q %d/%d cost=%d calls=%v", out, in, outTok, cost, calls)
	}
}

// best-effort cost: when codex emits a cost field it becomes milli-USD.
func TestParseCodexJSONL_Cost(t *testing.T) {
	jsonl := `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1,"total_cost_usd":0.012}}`
	_, _, _, cost, _ := parseCodexJSONL([]byte(jsonl))
	if cost != 12 {
		t.Errorf("cost = %d milli-USD, want 12 (0.012 USD)", cost)
	}
}
