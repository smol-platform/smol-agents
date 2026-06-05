package harness

import (
	"encoding/json"
	"math"
)

// claudeResult mirrors the `claude --output-format json` envelope (only the
// fields we consume; unknown fields are ignored so schema drift between weekly
// claude-code releases degrades gracefully rather than breaking the parse).
type claudeResult struct {
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	SessionID    string  `json:"session_id"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// parseClaudeJSON parses the claude-code JSON-output envelope into (output text,
// tokensIn, tokensOut, costMilli, sessionID). It is only called when the harness
// requested --output-format json. On a malformed envelope it returns the raw
// bytes as output with zero usage — the run degrades to opaque output instead of
// failing. Cost is rounded to integer milli-USD (observability only).
func parseClaudeJSON(stdout []byte) (output []byte, tokensIn, tokensOut, costMilli int64, sessionID string) {
	var r claudeResult
	if err := json.Unmarshal(stdout, &r); err != nil {
		return stdout, 0, 0, 0, ""
	}
	return []byte(r.Result), r.Usage.InputTokens, r.Usage.OutputTokens,
		int64(math.Round(r.TotalCostUSD * 1000)), r.SessionID
}
