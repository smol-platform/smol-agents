package harness

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	v1 "github.com/stigen/smol-agents/pkg/agentmodel/v1"
)

// fakeCmd returns an exec.Cmd that runs `/bin/sh -c "echo $args"`. We
// inject this in CLI harness tests so they don't need the real binaries.
func fakeCmd(echo string) commandFunc {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Render the args into a single echo line so output is predictable.
		_ = name
		_ = args
		return exec.CommandContext(ctx, "/bin/sh", "-c", "echo "+echo)
	}
}

func TestRegistry_AllKindsRegistered(t *testing.T) {
	r := Default()
	for _, k := range []v1.HarnessKind{
		v1.HarnessClaudeCode, v1.HarnessCodex, v1.HarnessAider, v1.HarnessGoose,
		v1.HarnessGenericCLI, v1.HarnessPi, v1.HarnessGenericHTTP,
	} {
		if _, err := r.For(k); err != nil {
			t.Errorf("missing impl for %s: %v", k, err)
		}
	}
}

func TestRegistry_UnknownKindErrors(t *testing.T) {
	r := Default()
	if _, err := r.For("nope"); err == nil {
		t.Error("expected error for unknown kind")
	}
}

func TestPromptFromInput(t *testing.T) {
	cases := map[string]string{
		`{"prompt":"hi"}`:                     "hi",
		`{"question":"q"}`:                    "q",
		`{"input":"hello"}`:                   "hello",
		`"plain"`:                             "plain",
		``:                                    "",
		`{"unrelated":"x","prompt":"chosen"}`: "chosen",
	}
	for in, want := range cases {
		got := promptFromInput(json.RawMessage(in))
		if got != want {
			t.Errorf("promptFromInput(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapWriter(t *testing.T) {
	w := &capWriter{limit: 5}
	n, _ := w.Write([]byte("hello world"))
	if n != 11 {
		t.Errorf("Write returned %d, want 11", n)
	}
	if got := string(w.Bytes()); got != "hello" {
		t.Errorf("Bytes() = %q, want hello", got)
	}
}

func TestClaudeCodeHarness_RunsCommand(t *testing.T) {
	h := &ClaudeCodeHarness{Cmd: fakeCmd("claude-output")}
	resp, err := h.Run(context.Background(), Request{
		Spec:   v1.HarnessSpec{Kind: v1.HarnessClaudeCode},
		Input:  json.RawMessage(`{"prompt":"hello"}`),
		Budget: v1.Budget{MaxWallClockSeconds: 5, MaxSteps: 1, MaxTokens: 1, MaxToolCalls: 0},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(resp.Output), "claude-output") {
		t.Errorf("output missing expected: %q", resp.Output)
	}
}

func TestCodexHarness_RunsCommand(t *testing.T) {
	h := &CodexHarness{Cmd: fakeCmd("codex-output")}
	resp, _ := h.Run(context.Background(), Request{
		Spec:   v1.HarnessSpec{Kind: v1.HarnessCodex},
		Input:  json.RawMessage(`{"prompt":"hi"}`),
		Budget: v1.Budget{MaxWallClockSeconds: 5, MaxSteps: 1, MaxTokens: 1, MaxToolCalls: 0},
	})
	if !strings.Contains(string(resp.Output), "codex-output") {
		t.Errorf("output: %q", resp.Output)
	}
}

func TestPiHarness_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		if got["context"] != "hi" {
			t.Errorf("expected context=hi, got %v", got)
		}
		_, _ = w.Write([]byte(`{"text":"answer from pi"}`))
	}))
	defer srv.Close()

	h := &PiHarness{Client: srv.Client()}
	resp, err := h.Run(context.Background(), Request{
		Spec:   v1.HarnessSpec{Kind: v1.HarnessPi, HTTP: &v1.HarnessHTTPSpec{URL: srv.URL}},
		Input:  json.RawMessage(`{"prompt":"hi"}`),
		Budget: v1.Budget{MaxWallClockSeconds: 5, MaxSteps: 1, MaxTokens: 1, MaxToolCalls: 0},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(resp.Output) != "answer from pi" {
		t.Errorf("output = %q", resp.Output)
	}
}

func TestGenericHTTPHarness_DottedPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"deep answer"}}]}`))
	}))
	defer srv.Close()

	h := &GenericHTTPHarness{Client: srv.Client()}
	resp, err := h.Run(context.Background(), Request{
		Spec: v1.HarnessSpec{
			Kind: v1.HarnessGenericHTTP,
			HTTP: &v1.HarnessHTTPSpec{URL: srv.URL, ResponseField: "choices.0.message.content"},
		},
		Input:  json.RawMessage(`{"prompt":"go deep"}`),
		Budget: v1.Budget{MaxWallClockSeconds: 5, MaxSteps: 1, MaxTokens: 1, MaxToolCalls: 0},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(resp.Output) != "deep answer" {
		t.Errorf("output = %q", resp.Output)
	}
}

func TestGenericHTTPHarness_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`internal`))
	}))
	defer srv.Close()
	h := &GenericHTTPHarness{Client: srv.Client()}
	_, err := h.Run(context.Background(), Request{
		Spec:  v1.HarnessSpec{Kind: v1.HarnessGenericHTTP, HTTP: &v1.HarnessHTTPSpec{URL: srv.URL}},
		Input: json.RawMessage(`{"prompt":"x"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 error: %v", err)
	}
}

func TestExtractField_Variants(t *testing.T) {
	body := []byte(`{"a":{"b":"x"},"arr":["zero","one","two"]}`)
	if got := extractField(body, "a.b"); got != "x" {
		t.Errorf("nested = %q", got)
	}
	if got := extractField(body, "arr.1"); got != "one" {
		t.Errorf("array index = %q", got)
	}
	if got := extractField(body, ""); got != string(body) {
		t.Errorf("empty path != raw")
	}
}

func TestRunCLI_BudgetTimeout(t *testing.T) {
	h := &ClaudeCodeHarness{
		Cmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 5")
		},
	}
	_, err := h.Run(context.Background(), Request{
		Spec:   v1.HarnessSpec{Kind: v1.HarnessClaudeCode},
		Input:  json.RawMessage(`{"prompt":"hi"}`),
		Budget: v1.Budget{MaxWallClockSeconds: 1, MaxSteps: 1, MaxTokens: 1, MaxToolCalls: 0},
	})
	if err == nil {
		t.Fatal("expected timeout")
	}
}
