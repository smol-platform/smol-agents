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

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
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
		v1.HarnessGenericCLI, v1.HarnessPi, v1.HarnessGenericHTTP, v1.HarnessHermes,
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

func TestImagesFromInput(t *testing.T) {
	// URL passthrough, b64 -> data URL, default mime; order preserved.
	got := imagesFromInput(json.RawMessage(
		`{"prompt":"hi","images":[{"url":"https://x/a.png"},{"b64":"AAAA","mime":"image/jpeg"},{"b64":"BBBB"}]}`))
	want := []string{"https://x/a.png", "data:image/jpeg;base64,AAAA", "data:image/png;base64,BBBB"}
	if len(got) != len(want) {
		t.Fatalf("got %d images, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("image[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// No images / non-object input -> nil (text-only path unaffected).
	if imagesFromInput(json.RawMessage(`{"prompt":"hi"}`)) != nil {
		t.Error("absent images key should yield nil")
	}
	if imagesFromInput(json.RawMessage(`"plain"`)) != nil {
		t.Error("non-object input should yield nil")
	}
}

func TestScreenImages(t *testing.T) {
	dataURI := "data:image/png;base64,AAAA"
	allow := &v1.ImagePolicy{AllowURLs: true}
	cases := []struct {
		name    string
		images  []string
		policy  *v1.ImagePolicy
		wantErr string // substring; "" means no error
		wantLen int
	}{
		{"data uri always allowed (nil policy)", []string{dataURI}, nil, "", 1},
		{"http denied by default (nil policy)", []string{"https://x/a.png"}, nil, "disabled", 0},
		{"http denied when AllowURLs false", []string{"https://x/a.png"}, &v1.ImagePolicy{}, "disabled", 0},
		{"http allowed when opted in", []string{"https://cdn.example.com/a.png"}, allow, "", 1},
		{"cloud metadata ip blocked even when allowed", []string{"http://169.254.169.254/latest/meta-data/"}, allow, "private/internal", 0},
		{"loopback blocked", []string{"http://127.0.0.1/x"}, allow, "private/internal", 0},
		{"ipv6 loopback blocked", []string{"http://[::1]/x"}, allow, "private/internal", 0},
		{"private range blocked", []string{"http://10.0.0.5/x"}, allow, "private/internal", 0},
		{"localhost blocked", []string{"http://localhost/x"}, allow, "private/internal", 0},
		{"internal name blocked", []string{"http://metadata.google.internal/x"}, allow, "private/internal", 0},
		{"host allow-list permits listed", []string{"https://cdn.example.com/a.png"}, &v1.ImagePolicy{AllowURLs: true, AllowedURLHosts: []string{"cdn.example.com"}}, "", 1},
		{"host allow-list rejects unlisted", []string{"https://evil.com/a.png"}, &v1.ImagePolicy{AllowURLs: true, AllowedURLHosts: []string{"cdn.example.com"}}, "allowedURLHosts", 0},
		{"non-http scheme rejected", []string{"file:///etc/passwd"}, allow, "scheme", 0},
		{"denied http fails loud even beside an allowed data uri", []string{dataURI, "https://x/a.png"}, nil, "disabled", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := screenImages(tc.images, tc.policy)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(got) != tc.wantLen {
				t.Errorf("got %d images, want %d", len(got), tc.wantLen)
			}
		})
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

func TestHermesHarness(t *testing.T) {
	var gotBody map[string]any
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		gotHeaders = r.Header.Clone()
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello from hermes"}}],` +
			`"usage":{"prompt_tokens":12,"completion_tokens":34}}`))
	}))
	defer srv.Close()

	h := &HermesHarness{Client: srv.Client()}
	resp, err := h.Run(context.Background(), Request{
		Spec: v1.HarnessSpec{
			Kind:          v1.HarnessHermes,
			SessionPolicy: v1.SessionPersistent,
			HTTP:          &v1.HarnessHTTPSpec{URL: srv.URL},
			// Literal config travels in Spec.Env (what the executor passes today);
			// the bearer token is a secretRef the executor resolves into Request.Env.
			Env: []v1.HarnessEnvVar{
				{Name: "HERMES_SESSION_ID", Value: "sess-7"},
				{Name: "HERMES_MODEL", Value: "hermes-4-405b"},
				{Name: "BODY_temperature", Value: "0.5"},
				{Name: "HEADER_Authorization", SecretRef: &v1.AuthRef{SecretName: "hermes-gateway"}},
			},
		},
		Instructions: "be terse",
		Input:        json.RawMessage(`{"prompt":"hi"}`),
		// Broker-resolved secret (executor fills Request.Env); overrides the literal.
		Env:    map[string]string{"HEADER_Authorization": "Bearer sk-xyz"},
		Budget: v1.Budget{MaxWallClockSeconds: 5, MaxTokens: 4096},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Response + real token accounting from usage.
	if string(resp.Output) != "hello from hermes" {
		t.Errorf("output = %q", resp.Output)
	}
	if resp.TokensIn != 12 || resp.TokensOut != 34 {
		t.Errorf("usage tokens = %d/%d, want 12/34", resp.TokensIn, resp.TokensOut)
	}
	// Model override + budget-derived max_tokens + JSON-typed BODY_ extra.
	if gotBody["model"] != "hermes-4-405b" {
		t.Errorf("model = %v", gotBody["model"])
	}
	if gotBody["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v", gotBody["max_tokens"])
	}
	if gotBody["temperature"] != 0.5 {
		t.Errorf("temperature = %v (%T), want 0.5 number", gotBody["temperature"], gotBody["temperature"])
	}
	// messages = [system(instructions), user(prompt)].
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}
	m0, _ := msgs[0].(map[string]any)
	m1, _ := msgs[1].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "be terse" {
		t.Errorf("system message = %v", m0)
	}
	if m1["role"] != "user" || m1["content"] != "hi" {
		t.Errorf("user message = %v", m1)
	}
	// Auth + session-continuity headers.
	if gotHeaders.Get("Authorization") != "Bearer sk-xyz" {
		t.Errorf("Authorization = %q", gotHeaders.Get("Authorization"))
	}
	if gotHeaders.Get("X-Hermes-Session-Id") != "sess-7" {
		t.Errorf("X-Hermes-Session-Id = %q", gotHeaders.Get("X-Hermes-Session-Id"))
	}
}

// Ephemeral runs MUST send a unique X-Hermes-Session-Id per run. Omitting it is
// not stateless: the gateway derives a session from a hash of the system prompt
// + first user message and reuses it, so repeated identical prompts accumulate
// one ever-growing conversation until it overflows the context window and the
// gateway returns empty output. A fresh id per run keeps each run isolated.
func TestHermesHarness_EphemeralUsesUniqueSession(t *testing.T) {
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids = append(ids, r.Header.Get("X-Hermes-Session-Id"))
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	h := &HermesHarness{Client: srv.Client()}
	req := Request{
		Spec: v1.HarnessSpec{
			Kind: v1.HarnessHermes, SessionPolicy: v1.SessionEphemeral,
			HTTP: &v1.HarnessHTTPSpec{URL: srv.URL},
			// Even if a HERMES_SESSION_ID literal is present, ephemeral must not
			// reuse it (that is the persistent-mode knob).
			Env: []v1.HarnessEnvVar{{Name: "HERMES_SESSION_ID", Value: "should-not-send"}},
		},
		Input: json.RawMessage(`{"prompt":"x"}`),
	}
	for i := 0; i < 2; i++ {
		if _, err := h.Run(context.Background(), req); err != nil {
			t.Fatalf("Run #%d: %v", i, err)
		}
	}

	if len(ids) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(ids))
	}
	for _, id := range ids {
		if id == "" {
			t.Fatal("ephemeral run omitted X-Hermes-Session-Id (lets the gateway reuse a content-hash session)")
		}
		if !strings.HasPrefix(id, "api-eph-") {
			t.Errorf("session id %q lacks the api-eph- prefix", id)
		}
		if id == "should-not-send" {
			t.Error("ephemeral run reused the HERMES_SESSION_ID literal (that is persistent-only)")
		}
	}
	if ids[0] == ids[1] {
		t.Errorf("two ephemeral runs shared session id %q; each run must be isolated", ids[0])
	}
}

func TestHermesHarness_RequiresURL(t *testing.T) {
	h := &HermesHarness{}
	if _, err := h.Run(context.Background(), Request{Spec: v1.HarnessSpec{Kind: v1.HarnessHermes}}); err == nil {
		t.Error("expected error when http.url is missing")
	}
}

// H3: an "images" array in the Run input becomes OpenAI multimodal content parts
// (text + image_url) on the user message; text-only inputs stay a plain string.
func TestHermesHarness_MultimodalImage(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"saw it"}}]}`))
	}))
	defer srv.Close()

	h := &HermesHarness{Client: srv.Client()}
	_, err := h.Run(context.Background(), Request{
		Spec:  v1.HarnessSpec{Kind: v1.HarnessHermes, HTTP: &v1.HarnessHTTPSpec{URL: srv.URL, ImagePolicy: &v1.ImagePolicy{AllowURLs: true}}},
		Input: json.RawMessage(`{"prompt":"what is this?","images":[{"url":"https://x/a.png"}]}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs, _ := gotBody["messages"].([]any)
	user, _ := msgs[len(msgs)-1].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("user content should be an array of parts, got %T: %v", user["content"], user["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 parts (text+image), got %d: %v", len(parts), parts)
	}
	if p0, _ := parts[0].(map[string]any); p0["type"] != "text" || p0["text"] != "what is this?" {
		t.Errorf("text part = %v", parts[0])
	}
	p1, _ := parts[1].(map[string]any)
	iu, _ := p1["image_url"].(map[string]any)
	if p1["type"] != "image_url" || iu["url"] != "https://x/a.png" {
		t.Errorf("image part = %v", p1)
	}
}

// H3 SSRF gating: an http(s) image URL is denied by default (no imagePolicy) and
// the run fails BEFORE the gateway is called — the gateway must never be asked
// to fetch an unvetted URL.
func TestHermesHarness_ImageURLDeniedByDefault(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	h := &HermesHarness{Client: srv.Client()}
	_, err := h.Run(context.Background(), Request{
		Spec:  v1.HarnessSpec{Kind: v1.HarnessHermes, HTTP: &v1.HarnessHTTPSpec{URL: srv.URL}},
		Input: json.RawMessage(`{"prompt":"hi","images":[{"url":"https://x/a.png"}]}`),
	})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v, want http(s) image denied by default", err)
	}
	if called {
		t.Error("gateway must NOT be called when an image url is rejected")
	}
}

// O2(A): the Run's seed is forwarded as the OpenAI `seed` request field.
func TestHermesHarness_Seed(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	h := &HermesHarness{Client: srv.Client()}
	if _, err := h.Run(context.Background(), Request{
		Spec:  v1.HarnessSpec{Kind: v1.HarnessHermes, HTTP: &v1.HarnessHTTPSpec{URL: srv.URL}},
		Input: json.RawMessage(`{"prompt":"hi"}`),
		Seed:  7,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotBody["seed"] != float64(7) {
		t.Errorf("seed = %v, want 7", gotBody["seed"])
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
