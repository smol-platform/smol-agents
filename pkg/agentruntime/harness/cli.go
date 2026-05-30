package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
	c.Stdout = out
	c.Stderr = io.Discard

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
		return Response{Output: out.Bytes(), DurationMs: dur}, fmt.Errorf("harness: %w", err)
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

// mergeEnv combines the spec's static env, secret-fetched values, and
// the executor's resolved Env into the slice form exec.Cmd expects.
func mergeEnv(req Request) []string {
	out := []string{}
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

// imagesFromInput extracts image references from the JSON input's optional
// "images" array. Each entry is {"url":"..."} (an http(s) or data: URL passed
// through) or {"b64":"...","mime":"image/png"} (assembled into a data: URL).
// Returns nil when there are no images, so the text-only path is unaffected.
//
// Delivery note: images ride inside AgentRun.Input, which the operator marshals
// into a ~1 MiB ConfigMap — so a large inline b64 image overflows that cap
// before the pod starts. Prefer a URL for real images; keep inline b64 small.
func imagesFromInput(in json.RawMessage) []string {
	if len(in) == 0 {
		return nil
	}
	var m struct {
		Images []struct {
			URL  string `json:"url"`
			B64  string `json:"b64"`
			Mime string `json:"mime"`
		} `json:"images"`
	}
	if err := json.Unmarshal(in, &m); err != nil {
		return nil
	}
	var out []string
	for _, img := range m.Images {
		switch {
		case img.URL != "":
			out = append(out, img.URL)
		case img.B64 != "":
			mime := img.Mime
			if mime == "" {
				mime = "image/png"
			}
			out = append(out, "data:"+mime+";base64,"+img.B64)
		}
	}
	return out
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
	if req.Instructions != "" {
		prompt = req.Instructions + "\n\n" + prompt
	}
	args := []string{flag, prompt}
	return runCLI(ctx, req, "claude", args, h.Cmd)
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
	return runCLI(ctx, req, "codex", []string{"exec", prompt}, h.Cmd)
}

// AiderHarness invokes `aider --message "<prompt>" --no-pretty --yes`.
type AiderHarness struct {
	Cmd commandFunc
}

func (h *AiderHarness) Kind() v1.HarnessKind { return v1.HarnessAider }

func (h *AiderHarness) Run(ctx context.Context, req Request) (Response, error) {
	args := []string{"--message", promptFromInput(req.Input), "--no-pretty", "--yes"}
	return runCLI(ctx, req, "aider", args, h.Cmd)
}

// GooseHarness invokes `goose run --instructions "<prompt>"`.
type GooseHarness struct {
	Cmd commandFunc
}

func (h *GooseHarness) Kind() v1.HarnessKind { return v1.HarnessGoose }

func (h *GooseHarness) Run(ctx context.Context, req Request) (Response, error) {
	args := []string{"run", "--instructions", promptFromInput(req.Input)}
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
	// runCLI already merges spec.Command — pass an empty name and let it
	// take the override.
	return runCLI(ctx, req, "", args, h.Cmd)
}
