package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M4.16: pi runs in JSON mode; session flags map ephemeral→--no-session,
// resume→--session <id> (M4.19); model/system/extra are forwarded; the prompt is
// the final -p arg.
func TestPiArgs(t *testing.T) {
	a := piArgs(runRequest{Prompt: "hi", Model: "m1", System: "be brief"}, []string{"--verbose"})
	got := strings.Join(a, " ")
	for _, want := range []string{"--mode json", "--no-session", "--model m1", "--system be brief", "--verbose", "-p hi"} {
		if !strings.Contains(got, want) {
			t.Errorf("piArgs missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "--session ") {
		t.Errorf("ephemeral run must not pass --session: %q", got)
	}

	// Resume: --session <id>, no --no-session.
	r := piArgs(runRequest{Prompt: "x", SessionID: "sess-9"}, nil)
	rs := strings.Join(r, " ")
	if !strings.Contains(rs, "--session sess-9") || strings.Contains(rs, "--no-session") {
		t.Errorf("resume args wrong: %q", rs)
	}
}

// M4.16: the spawned pi env must carry NO *_API_KEY (the key lives in the 0600
// models.json instead).
func TestScrubbedEnv(t *testing.T) {
	in := []string{"PATH=/bin", "PI_API_KEY=secret", "OPENAI_API_KEY=s2", "HOME=/h", "NOTKEY=ok"}
	out := scrubbedEnv(in)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "API_KEY") {
		t.Errorf("scrubbed env still has an API key: %v", out)
	}
	for _, want := range []string{"PATH=/bin", "HOME=/h", "NOTKEY=ok"} {
		if !strings.Contains(joined, want) {
			t.Errorf("scrubbed env dropped a non-key var %q: %v", want, out)
		}
	}
}

// M4.16: JSONL is split on '\n' ONLY (a '\r' inside a token is preserved); text
// accumulates, the last usage block wins, tool events are collected.
func TestParsePiJSONL(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"text","text":"Hello"}`,
		`{"type":"tool_call","name":"bash"}`,
		"{\"type\":\"text\",\"text\":\"line1\\rstillline1\"}", // embedded \r must survive
		`{"type":"usage","usage":{"input_tokens":12,"output_tokens":7}}`,
		`not json — ignored`,
		`{"type":"final","text":"!"}`,
	}, "\n")
	resp := parsePiJSONL(strings.NewReader(stream))
	if resp.Output != "Helloline1\rstillline1!" {
		t.Errorf("Output = %q", resp.Output)
	}
	if resp.TokensIn != 12 || resp.TokensOut != 7 {
		t.Errorf("tokens = in:%d out:%d, want 12/7", resp.TokensIn, resp.TokensOut)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "bash" {
		t.Errorf("toolCalls = %+v", resp.ToolCalls)
	}
}

// M4.16 (e2e of the bridge with a fake pi): runPi spawns the binary with the
// scrubbed env, the argv pi sees matches piArgs, and JSONL is parsed back.
func TestRunPi_FakePi(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	envFile := filepath.Join(dir, "env")
	fakePi := filepath.Join(dir, "pi")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + argsFile + "\n" +
		"env > " + envFile + "\n" +
		`printf '%s\n' '{"type":"text","text":"done"}' '{"type":"usage","usage":{"tokensIn":3,"tokensOut":4}}'` + "\n"
	if err := os.WriteFile(fakePi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOO_API_KEY", "leak-me")

	resp, err := runPi(context.Background(), fakePi, runRequest{Prompt: "go", Model: "m"}, nil)
	if err != nil {
		t.Fatalf("runPi: %v", err)
	}
	if resp.Output != "done" || resp.TokensIn != 3 || resp.TokensOut != 4 {
		t.Errorf("resp = %+v", resp)
	}
	argv, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(argv), "--mode") || !strings.Contains(string(argv), "go") {
		t.Errorf("fake pi argv = %q", argv)
	}
	envOut, _ := os.ReadFile(envFile)
	if strings.Contains(string(envOut), "FOO_API_KEY") {
		t.Errorf("pi child saw an API key in its env:\n%s", envOut)
	}
}
