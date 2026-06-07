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
// tokensIn, tokensOut, costMilli, sessionID, isError). It is only called when the
// harness requested --output-format json. On a malformed envelope it returns the
// raw bytes as output with zero usage and isError=false — the run degrades to
// opaque output instead of failing. Cost is rounded to integer milli-USD
// (observability only). isError mirrors the envelope's is_error: claude exits 0
// even when the turn errored (tool failure, max turns), so the caller must
// surface it rather than report a silent success (M3.16).
func parseClaudeJSON(stdout []byte) (output []byte, tokensIn, tokensOut, costMilli int64, sessionID string, isError bool) {
	var r claudeResult
	if err := json.Unmarshal(stdout, &r); err != nil {
		return stdout, 0, 0, 0, "", false
	}
	return []byte(r.Result), r.Usage.InputTokens, r.Usage.OutputTokens,
		int64(math.Round(r.TotalCostUSD * 1000)), r.SessionID, r.IsError
}
