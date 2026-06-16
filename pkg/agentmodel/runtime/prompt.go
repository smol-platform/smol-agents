package runtime

import (
	"encoding/json"
	"strings"
)

// PromptFromInput extracts the prompt string from a run's input JSON. It is the
// single source of truth shared by the loop LLM (openaillm) and the CLI/HTTP
// harnesses, so the same input is read identically regardless of Agent.Mode
// (previously the two paths recognised slightly different key sets — "task" on
// the loop, "message" on harnesses — so one input could be read two ways).
//
// Resolution order: a bare JSON string is the prompt; otherwise the first
// present of prompt/question/input/task/message on a JSON object; otherwise the
// raw (trimmed) bytes.
func PromptFromInput(in json.RawMessage) string {
	if len(in) == 0 {
		return ""
	}
	// A bare JSON string is the prompt.
	var s string
	if json.Unmarshal(in, &s) == nil {
		return s
	}
	// An object: take the first recognised key.
	var m map[string]json.RawMessage
	if json.Unmarshal(in, &m) == nil {
		for _, k := range []string{"prompt", "question", "input", "task", "message"} {
			if v, ok := m[k]; ok {
				var sv string
				if json.Unmarshal(v, &sv) == nil {
					return sv
				}
			}
		}
	}
	// Fall back to the raw bytes (trimmed).
	return strings.TrimSpace(string(in))
}
