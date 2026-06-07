package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
)

// maxOutputBytes bounds the accumulated assistant text the bridge returns, so a
// runaway pi run can't balloon the response.
const maxOutputBytes = 1 << 20 // 1 MiB

// runRequest is the bridge's POST /run body (from PiMonoHarness).
type runRequest struct {
	Prompt    string `json:"prompt"`
	System    string `json:"system,omitempty"`
	Model     string `json:"model,omitempty"`
	Seed      int64  `json:"seed,omitempty"`
	SessionID string `json:"sessionID,omitempty"` // M4.19: resume a pi session
}

// runResponse mirrors the harness's piBridgeResponse.
type runResponse struct {
	Output    string     `json:"output"`
	TokensIn  int64      `json:"tokensIn"`
	TokensOut int64      `json:"tokensOut"`
	ToolCalls []toolCall `json:"toolCalls"`
}

type toolCall struct {
	Name string `json:"name"`
}

// piArgs builds the pi argv for a request. pi runs in JSON mode (the only mode
// that yields parseable tokens + tool-calls). Session flags (M4.19): a SessionID
// resumes that session; otherwise --no-session keeps the run ephemeral.
func piArgs(req runRequest, extra []string) []string {
	args := []string{"--mode", "json"}
	if req.SessionID != "" {
		args = append(args, "--session", req.SessionID)
	} else {
		args = append(args, "--no-session")
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.System != "" {
		args = append(args, "--system", req.System)
	}
	args = append(args, extra...)
	args = append(args, "-p", req.Prompt)
	return args
}

// scrubbedEnv returns the environment with every *_API_KEY removed, so the
// spawned pi process never sees a provider key in its env — pi reads it from the
// 0600 models.json the bridge wrote. Defense-in-depth (NOT airtight: a same-uid
// process can read the file); the microVM + egress cage are the real controls.
func scrubbedEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		if strings.HasSuffix(name, "_API_KEY") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// parsePiJSONL stream-parses pi's --mode json output. pi emits one JSON object
// per line; we split on '\n' ONLY (pi tokens may contain '\r'). We accumulate
// assistant text, keep the LAST usage block, and collect tool-call events.
func parsePiJSONL(r io.Reader) runResponse {
	var resp runResponse
	var text strings.Builder
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sc.Split(scanLinesLF)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue // ignore non-JSON noise
		}
		switch jsonString(ev["type"]) {
		case "text", "assistant", "content":
			if text.Len() < maxOutputBytes {
				text.WriteString(jsonString(ev["text"]))
			}
		case "tool_call", "tool_use":
			resp.ToolCalls = append(resp.ToolCalls, toolCall{Name: jsonString(ev["name"])})
		case "usage", "result", "final":
			if u := ev["usage"]; u != nil {
				ti, to := parseUsage(u)
				resp.TokensIn, resp.TokensOut = ti, to
			}
			if t := jsonString(ev["text"]); t != "" && text.Len() < maxOutputBytes {
				text.WriteString(t)
			}
		}
	}
	resp.Output = text.String()
	return resp
}

// scanLinesLF is a bufio.SplitFunc that splits on '\n' only (NOT '\r\n'), so a
// '\r' inside a pi token is preserved (M4.16).
func scanLinesLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		if b == '\n' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func parseUsage(raw json.RawMessage) (in, out int64) {
	var u struct {
		TokensIn     int64 `json:"tokensIn"`
		TokensOut    int64 `json:"tokensOut"`
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		PromptTokens int64 `json:"prompt_tokens"`
		Completion   int64 `json:"completion_tokens"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return 0, 0
	}
	in = firstNonZero(u.TokensIn, u.InputTokens, u.PromptTokens)
	out = firstNonZero(u.TokensOut, u.OutputTokens, u.Completion)
	return in, out
}

func firstNonZero(vals ...int64) int64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func jsonString(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// runPi spawns pi with the scrubbed env and returns the parsed response. ctx
// cancellation kills the subprocess.
func runPi(ctx context.Context, piBin string, req runRequest, extra []string) (runResponse, error) {
	cmd := exec.CommandContext(ctx, piBin, piArgs(req, extra)...)
	cmd.Env = scrubbedEnv(os.Environ())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runResponse{}, err
	}
	if err := cmd.Start(); err != nil {
		return runResponse{}, err
	}
	resp := parsePiJSONL(stdout)
	err = cmd.Wait()
	return resp, err
}
