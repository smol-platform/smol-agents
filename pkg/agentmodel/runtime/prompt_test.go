package runtime

import (
	"encoding/json"
	"testing"
)

func TestPromptFromInput(t *testing.T) {
	cases := map[string]string{
		`{"prompt":"hi"}`:                     "hi",
		`{"question":"q"}`:                    "q",
		`{"input":"hello"}`:                   "hello",
		`{"task":"do it"}`:                    "do it",    // loop-path key
		`{"message":"hey"}`:                   "hey",      // harness-path key
		`"plain"`:                             "plain",    // bare JSON string
		``:                                    "",         // empty
		`{"unrelated":"x","prompt":"chosen"}`: "chosen",   // first recognised key wins
		`not json`:                            "not json", // raw fallback (trimmed)
	}
	for in, want := range cases {
		if got := PromptFromInput(json.RawMessage(in)); got != want {
			t.Errorf("PromptFromInput(%q) = %q, want %q", in, got, want)
		}
	}

	// Precedence: prompt beats the lower-priority keys when several are present.
	if got := PromptFromInput(json.RawMessage(`{"message":"m","prompt":"p"}`)); got != "p" {
		t.Errorf("precedence: got %q, want p", got)
	}
}
