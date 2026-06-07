package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// codexEvent is one line of `codex exec --json` output. codex emits a stream of
// newline-delimited thread events; we read only the fields we consume and ignore
// unknown event types + fields so codex's evolving JSON schema degrades
// gracefully (like the claude parser). Pinned to the codex thread-event shape
// (type/item/usage) — a major codex schema change needs a parser update, so the
// caller treats a no-event parse as opaque output rather than failing.
type codexEvent struct {
	Type  string      `json:"type"`
	Item  *codexItem  `json:"item"`
	Usage *codexUsage `json:"usage"`
}

type codexItem struct {
	Type string `json:"type"` // agent_message | command_execution | mcp_tool_call | ...
	Text string `json:"text"` // agent_message text
	// command_execution
	Command          string `json:"command"`
	ExitCode         *int   `json:"exit_code"`
	AggregatedOutput string `json:"aggregated_output"`
	// mcp_tool_call / function_call
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Error     string          `json:"error"`
}

type codexUsage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`       // best-effort; codex rarely emits cost
	TotalCostUSD float64 `json:"total_cost_usd"` // alternate key
}

// parseCodexJSONL parses `codex exec --json` JSONL into (output, tokensIn,
// tokensOut, costMilli, toolCalls). Best-effort: it scans line by line, skips any
// unparseable line (tolerating a truncated final line), takes the last non-zero
// usage block (a single codex exec is one turn) and the LAST agent_message as the
// output, and maps command/tool-execution items to ToolCallRecords. If no line
// parses as a codex event it returns the raw bytes as output with zero usage
// (degrade to opaque output, never panic). Cost → integer milli-USD.
func parseCodexJSONL(stdout []byte) (output []byte, tokensIn, tokensOut, costMilli int64, toolCalls []v1.ToolCallRecord) {
	var (
		lastMsg  string
		gotMsg   bool
		anyEvent bool
	)
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20) // tolerate long lines (bounded by runCLI's cap)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // tolerate a partial / garbled line
		}
		anyEvent = true
		if ev.Usage != nil {
			if ev.Usage.InputTokens > 0 {
				tokensIn = ev.Usage.InputTokens
			}
			if ev.Usage.OutputTokens > 0 {
				tokensOut = ev.Usage.OutputTokens
			}
			if c := ev.Usage.CostUSD; c > 0 {
				costMilli = int64(math.Round(c * 1000))
			} else if c := ev.Usage.TotalCostUSD; c > 0 {
				costMilli = int64(math.Round(c * 1000))
			}
		}
		if ev.Item == nil {
			continue
		}
		switch ev.Item.Type {
		case "agent_message", "assistant_message", "message":
			if ev.Item.Text != "" {
				lastMsg, gotMsg = ev.Item.Text, true
			}
		case "command_execution", "local_shell_call", "exec_command":
			rec := v1.ToolCallRecord{Tool: "exec"}
			if ev.Item.Command != "" {
				rec.Arguments, _ = json.Marshal(map[string]string{"command": ev.Item.Command})
			}
			if ev.Item.AggregatedOutput != "" {
				rec.Result, _ = json.Marshal(ev.Item.AggregatedOutput)
			}
			if ev.Item.ExitCode != nil && *ev.Item.ExitCode != 0 {
				rec.Error = fmt.Sprintf("exit code %d", *ev.Item.ExitCode)
			}
			toolCalls = append(toolCalls, rec)
		case "mcp_tool_call", "function_call", "tool_call":
			name := ev.Item.Name
			if name == "" {
				name = "tool"
			}
			toolCalls = append(toolCalls, v1.ToolCallRecord{
				Tool: name, Arguments: ev.Item.Arguments, Result: ev.Item.Result, Error: ev.Item.Error,
			})
		}
	}
	if !anyEvent {
		return stdout, 0, 0, 0, nil // not codex JSONL → opaque output
	}
	if gotMsg {
		output = []byte(lastMsg)
	} else {
		output = stdout // events but no final message → keep raw for debuggability
	}
	return output, tokensIn, tokensOut, costMilli, toolCalls
}
