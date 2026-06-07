package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// commandFunc is swapped by tests to return a predictable command.
type commandFunc func(ctx context.Context, name string, args ...string) *exec.Cmd

func defaultCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// runCLI is the shared subprocess driver. It captures stdout (bounded),
// honours ctx cancellation, and returns a Response. Used by every
// CLI-shaped harness.
func runCLI(ctx context.Context, req Request, name string, args []string, cmd commandFunc) (Response, error) {
	if cmd == nil {
		cmd = defaultCommand
	}
	if req.Spec.Command != nil {
		// Tenant override: the first element is the binary, the rest
		// are args appended to the kind-specific defaults.
		name = req.Spec.Command[0]
		args = append(append([]string{}, req.Spec.Command[1:]...), args...)
	}

	ctx, cancel := budgetTimeout(ctx, req.Budget)
	defer cancel()

	c := cmd(ctx, name, args...)
	if req.WorkingDir != "" {
		c.Dir = req.WorkingDir
	} else if req.Spec.CLI != nil && req.Spec.CLI.WorkingDir != "" {
		c.Dir = req.Spec.CLI.WorkingDir
	}
	c.Env = mergeEnv(req)

	maxBytes := int64(1 << 20)
	if req.Spec.CLI != nil && req.Spec.CLI.MaxOutputBytes > 0 {
		maxBytes = int64(req.Spec.CLI.MaxOutputBytes)
	}

	out := &capWriter{limit: maxBytes}
	// Capture stderr (bounded) for diagnostics: a failing CLI (bad flag, missing
	// auth, crash, OOM) writes the reason there — discarding it (the old
	// behavior) left run failures undiagnosable.
	errOut := &capWriter{limit: 8 << 10}
	c.Stdout = out
	c.Stderr = errOut

	startedAt := time.Now()
	err := c.Run()
	dur := time.Since(startedAt).Milliseconds()

	if ctx.Err() == context.DeadlineExceeded {
		return Response{Output: out.Bytes(), DurationMs: dur}, ErrTimeout
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return Response{Output: out.Bytes(), DurationMs: dur}, ErrCancelled
	}
	if err != nil {
		if msg := strings.TrimSpace(string(errOut.Bytes())); msg != "" {
			return Response{Output: out.Bytes(), DurationMs: dur}, fmt.Errorf("harness: %w: %s", err, msg)
		}
		return Response{Output: out.Bytes(), DurationMs: dur}, fmt.Errorf("harness: %w", err)
	}
	// Exit 0 with NOTHING on stdout but content on stderr: the CLI failed in a way
	// that didn't set a non-zero code (claude --print exits 0 on internal errors;
	// codex similar). Without this, the discarded stderr left the run folding as a
	// silent empty success — exactly the undiagnosable "Completed, empty output,
	// 0 tokens" we hit. Surface stderr so the run fails with the real reason.
	if len(bytes.TrimSpace(out.Bytes())) == 0 {
		if msg := strings.TrimSpace(string(errOut.Bytes())); msg != "" {
			return Response{Output: out.Bytes(), DurationMs: dur}, fmt.Errorf("harness: exited 0 with empty stdout: %s", msg)
		}
	}
	return Response{Output: out.Bytes(), DurationMs: dur}, nil
}

// capWriter buffers up to limit bytes; further writes are discarded
// silently. Tests rely on the panic-free, bounded shape.
type capWriter struct {
	buf   bytes.Buffer
	limit int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	free := c.limit - int64(c.buf.Len())
	if free <= 0 {
		return len(p), nil
	}
	if int64(len(p)) > free {
		c.buf.Write(p[:free])
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *capWriter) Bytes() []byte { return c.buf.Bytes() }

// mergeEnv combines the parent process environment (the image's HOME, PATH,
// etc. — without which CLIs like claude-code crash on uv_os_homedir / can't find
// their binaries) with the spec's static env and secret-fetched values, in the
// slice form exec.Cmd expects. Later entries win on duplicate keys, so the
// harness/secret env overrides the inherited env.
func mergeEnv(req Request) []string {
	out := append([]string{}, os.Environ()...)
	for k, v := range req.Env {
		out = append(out, k+"="+v)
	}
	for _, e := range req.Spec.Env {
		if e.Value != "" {
			out = append(out, e.Name+"="+e.Value)
		}
	}
	return out
}

// promptFromInput pulls the prompt string from the JSON input. If the
// input parses as an object with a "prompt" key, we use that;
// otherwise we use the raw input as text.
func promptFromInput(in json.RawMessage) string {
	if len(in) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(in, &m); err == nil {
		for _, k := range []string{"prompt", "question", "input", "message"} {
			if v, ok := m[k].(string); ok {
				return v
			}
		}
	}
	// Fall back to raw — strip surrounding quotes if it's a JSON string.
	s := strings.TrimSpace(string(in))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var u string
		if err := json.Unmarshal(in, &u); err == nil {
			return u
		}
	}
	return s
}

// cliExtraFlags returns the tenant-specified extra CLI flags
// (HarnessCLISpec.ExtraFlags) appended to a harness invocation — permission/
// sandbox flags a CLI needs in headless mode (e.g. claude --dangerously-skip-permissions).
func cliExtraFlags(req Request) []string {
	if req.Spec.CLI != nil {
		return req.Spec.CLI.ExtraFlags
	}
	return nil
}

// claudePermArgs maps the typed CLI permission posture (M3.14 fields) to claude
// flags (M3.17): ApprovalMode "acceptEdits" → --permission-mode acceptEdits;
// "never" → --dangerously-skip-permissions (the opt-in danger flag, which the
// operator admission gate refuses on a non-microVM runtime, D3); "" / "safe"
// keep the default headless posture. AllowedTools/DisallowedTools map to the
// claude permission allow/deny lists.
func claudePermArgs(req Request) []string {
	if req.Spec.CLI == nil {
		return nil
	}
	var args []string
	switch req.Spec.CLI.ApprovalMode {
	case "acceptEdits":
		args = append(args, "--permission-mode", "acceptEdits")
	case "never":
		args = append(args, "--dangerously-skip-permissions")
	}
	for _, t := range req.Spec.CLI.AllowedTools {
		args = append(args, "--allowedTools", t)
	}
	for _, t := range req.Spec.CLI.DisallowedTools {
		args = append(args, "--disallowedTools", t)
	}
	return args
}

// ClaudeCodeHarness invokes `claude --print "<prompt>"`.
type ClaudeCodeHarness struct {
	Cmd commandFunc // nil → exec.CommandContext
}

func (h *ClaudeCodeHarness) Kind() v1.HarnessKind { return v1.HarnessClaudeCode }

func (h *ClaudeCodeHarness) Run(ctx context.Context, req Request) (Response, error) {
	flag := "--print"
	if req.Spec.CLI != nil && req.Spec.CLI.PromptFlag != "" {
		flag = req.Spec.CLI.PromptFlag
	}
	prompt := promptFromInput(req.Input)
	// --output-format json lets us parse real tokens/cost/session-id (M2.5); the
	// flag goes before ExtraFlags + the prompt so a tenant override can't drop it.
	jsonOut := req.Spec.CLI != nil && req.Spec.CLI.OutputFormat == "json"
	args := []string{flag}
	if jsonOut {
		args = append(args, "--output-format", "json")
	}
	// Instructions belong in the system prompt, not stuffed into the user turn
	// (M3.16) — --append-system-prompt adds them to claude's system prompt.
	if req.Instructions != "" {
		args = append(args, "--append-system-prompt", req.Instructions)
	}
	// Resume a prior conversation when a session id was captured (M3.19); claude
	// reloads the transcript from its session store (HOME on AgentFS for durable).
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
	}
	// apiKeyHelper (M3.20): short-lived broker-leased creds. claude runs the helper
	// command (and re-runs it on TTL/401) instead of holding a static key.
	if req.Spec.CLI != nil && req.Spec.CLI.APIKeyHelperSecret != "" {
		settings, _ := json.Marshal(map[string]string{"apiKeyHelper": "/agent lease " + req.Spec.CLI.APIKeyHelperSecret})
		args = append(args, "--settings", string(settings))
	}
	// MCP servers (M3.18): point claude at the operator-rendered mcp-config and
	// auto-allow each server's mcp__<name>__* tools so they're usable headlessly.
	if req.Spec.CLI != nil && len(req.Spec.CLI.MCPServers) > 0 {
		args = append(args, "--mcp-config", v1.ClaudeMCPConfigPath)
		for _, s := range req.Spec.CLI.MCPServers {
			args = append(args, "--allowedTools", "mcp__"+s.Name+"__*")
		}
	}
	args = append(args, claudePermArgs(req)...)
	args = append(args, cliExtraFlags(req)...)
	args = append(args, prompt)

	resp, err := runCLI(ctx, req, "claude", args, h.Cmd)
	if err != nil || !jsonOut {
		return resp, err
	}
	out, in, outTok, costMilli, sessionID, isErr := parseClaudeJSON(resp.Output)
	resp.Output, resp.TokensIn, resp.TokensOut, resp.CostUSDMilli, resp.SessionID = out, in, outTok, costMilli, sessionID
	// claude --print exits 0 even when the turn errored (is_error). Surface it as
	// a harness error so the run is folded Failed, not a silent success (M3.16).
	// resp (with usage) is returned too, so token/cost accounting still lands.
	if isErr {
		return resp, fmt.Errorf("claude-code reported is_error: %s", errSnippet(out))
	}
	return resp, nil
}

// errSnippet trims harness output to a short, single-line diagnostic for an
// error message (the full output is already captured in the step).
func errSnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// CodexHarness invokes `codex exec "<prompt>"`.
type CodexHarness struct {
	Cmd commandFunc
}

func (h *CodexHarness) Kind() v1.HarnessKind { return v1.HarnessCodex }

func (h *CodexHarness) Run(ctx context.Context, req Request) (Response, error) {
	prompt := promptFromInput(req.Input)
	if req.Instructions != "" {
		prompt = req.Instructions + "\n\n" + prompt
	}
	// --json emits the thread-event stream we parse for real tokens/cost/tool
	// records (M2.6); it goes right after `exec` so a tenant override can't drop
	// it. Without OutputFormat=json codex behaves exactly as before (raw stdout).
	jsonOut := req.Spec.CLI != nil && req.Spec.CLI.OutputFormat == "json"

	// `exec`, or `exec resume <id>` to continue a persistent session (M3.23).
	args := []string{"exec"}
	if req.Spec.SessionPolicy == v1.SessionPersistent && req.SessionID != "" {
		args = append(args, "resume", req.SessionID)
	}
	if jsonOut {
		args = append(args, "--json")
	}
	// codex refuses to run outside a git repo; the AgentFS workspace isn't one.
	args = append(args, "--skip-git-repo-check")
	if req.WorkingDir != "" {
		args = append(args, "-C", req.WorkingDir)
	}
	// --output-last-message writes the final assistant message to a file we read
	// back for a reliable Output (more robust than scraping the JSONL stream).
	var lastMsgFile string
	if f, ferr := os.CreateTemp("", "codex-last-*.txt"); ferr == nil {
		lastMsgFile = f.Name()
		_ = f.Close()
		defer func() { _ = os.Remove(lastMsgFile) }()
		args = append(args, "--output-last-message", lastMsgFile)
	}
	args = append(args, codexApprovalArgs(req)...)
	args = append(args, cliExtraFlags(req)...)
	args = append(args, prompt)

	// Route codex through the platform Responses gateway (M3.21): copy the
	// operator-rendered config.toml into a writable CODEX_HOME (codex writes thread
	// state there, so it can't read it from the read-only run-spec mount) and make
	// sure the subprocess sees CODEX_HOME.
	if req.Spec.CLI != nil && req.Spec.CLI.CodexBaseURL != "" {
		home := os.Getenv("CODEX_HOME")
		if home == "" {
			home = "/tmp/.codex"
		}
		if err := os.MkdirAll(home, 0o700); err == nil {
			if cfg, rerr := os.ReadFile(v1.CodexConfigMountPath); rerr == nil {
				_ = os.WriteFile(filepath.Join(home, "config.toml"), cfg, 0o600)
			}
		}
		if req.Env == nil {
			req.Env = map[string]string{}
		}
		req.Env["CODEX_HOME"] = home
	}

	resp, err := runCLI(ctx, req, "codex", args, h.Cmd)
	if err != nil {
		return resp, err
	}
	if jsonOut {
		out, in, outTok, costMilli, calls := parseCodexJSONL(resp.Output)
		resp.Output, resp.TokensIn, resp.TokensOut, resp.CostUSDMilli, resp.ToolCalls = out, in, outTok, costMilli, calls
	}
	// Prefer codex's own last-message file when it wrote one (most reliable Output).
	if lastMsgFile != "" {
		if b, rerr := os.ReadFile(lastMsgFile); rerr == nil {
			if trimmed := bytes.TrimSpace(b); len(trimmed) > 0 {
				resp.Output = trimmed
			}
		}
	}
	return resp, nil
}

// codexApprovalArgs maps the shared ApprovalMode to codex's approval policy
// (M3.21): "never" → --ask-for-approval never, the opt-in headless posture
// (microVM-gated at admission, D3). Other modes leave codex's default approval
// policy in place. Codex's sandbox flag is a separate codex-spec concern.
func codexApprovalArgs(req Request) []string {
	if req.Spec.CLI != nil && req.Spec.CLI.ApprovalMode == "never" {
		return []string{"--ask-for-approval", "never"}
	}
	return nil
}

// AiderHarness invokes `aider --message "<prompt>" --no-pretty --yes`.
type AiderHarness struct {
	Cmd commandFunc
}

func (h *AiderHarness) Kind() v1.HarnessKind { return v1.HarnessAider }

func (h *AiderHarness) Run(ctx context.Context, req Request) (Response, error) {
	args := []string{"--message", promptFromInput(req.Input), "--no-pretty", "--yes"}
	args = append(args, cliExtraFlags(req)...)
	return runCLI(ctx, req, "aider", args, h.Cmd)
}

// GooseHarness invokes `goose run --instructions "<prompt>"`.
type GooseHarness struct {
	Cmd commandFunc
}

func (h *GooseHarness) Kind() v1.HarnessKind { return v1.HarnessGoose }

func (h *GooseHarness) Run(ctx context.Context, req Request) (Response, error) {
	args := []string{"run", "--instructions", promptFromInput(req.Input)}
	args = append(args, cliExtraFlags(req)...)
	return runCLI(ctx, req, "goose", args, h.Cmd)
}

// GenericCLIHarness lets tenants drive an arbitrary CLI. The CLI block
// MUST set a PromptFlag.
type GenericCLIHarness struct {
	Cmd commandFunc
}

func (h *GenericCLIHarness) Kind() v1.HarnessKind { return v1.HarnessGenericCLI }

func (h *GenericCLIHarness) Run(ctx context.Context, req Request) (Response, error) {
	if req.Spec.Command == nil || len(req.Spec.Command) == 0 {
		return Response{}, errors.New("harness: generic-cli requires spec.command")
	}
	flag := ""
	if req.Spec.CLI != nil {
		flag = req.Spec.CLI.PromptFlag
	}
	args := []string{}
	if flag != "" {
		args = append(args, flag, promptFromInput(req.Input))
	} else {
		args = append(args, promptFromInput(req.Input))
	}
	args = append(args, cliExtraFlags(req)...)
	// runCLI already merges spec.Command — pass an empty name and let it
	// take the override.
	return runCLI(ctx, req, "", args, h.Cmd)
}
