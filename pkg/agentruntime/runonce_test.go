package agentruntime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func writeSpec(t *testing.T, dir, name string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunOnce_HarnessHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi from server"}}],` +
			`"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	writeSpec(t, dir, AgentSpecFile, v1.Agent{Spec: v1.AgentSpec{
		Mode:    v1.ModeHarness,
		Budget:  v1.Budget{MaxSteps: 1, MaxTokens: 1000, MaxWallClockSeconds: 30},
		Harness: &v1.HarnessSpec{Kind: v1.HarnessHermes, HTTP: &v1.HarnessHTTPSpec{URL: srv.URL}},
	}})
	writeSpec(t, dir, RunSpecFile, v1.AgentRunSpec{Input: json.RawMessage(`{"prompt":"hey"}`), Seed: 1})

	res, err := RunOnce(context.Background(), dir, nil, nil)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// The harness content "hi from server" is plain text, not JSON, so it is
	// encoded as a JSON string — a raw non-JSON Output would make the RunResult
	// fail to marshal (the silent empty-output bug). AgentRun's status.output
	// accepts any JSON value, so the string is stored as the run's answer.
	if string(res.Output) != `"hi from server"` {
		t.Errorf("output = %q", res.Output)
	}
	if _, err := json.Marshal(ResultToWire(res, nil)); err != nil {
		t.Fatalf("RunResult must marshal: %v", err)
	}
	if res.Phase != v1.PhaseCompleted {
		t.Errorf("phase = %s, want Completed", res.Phase)
	}
}

func TestRunOnce_MissingSpec(t *testing.T) {
	if _, err := RunOnce(context.Background(), t.TempDir(), nil, nil); err == nil {
		t.Fatal("expected error when agent.json is absent")
	}
}

func TestResultToWire(t *testing.T) {
	w := ResultToWire(Result{
		Phase:  v1.PhaseCompleted,
		Output: json.RawMessage(`"ok"`),
		Steps:  []v1.Step{{Index: 0, Kind: v1.StepFinal}},
		Usage:  v1.Usage{Steps: 1},
	}, nil)
	if w.Phase != v1.PhaseCompleted || string(w.Output) != `"ok"` || w.Error != "" {
		t.Errorf("wire = %+v", w)
	}
	// Steps must travel on the wire (folded into AgentRun.Status.Steps).
	if len(w.Steps) != 1 || w.Steps[0].Kind != v1.StepFinal {
		t.Errorf("steps not carried to wire: %+v", w.Steps)
	}
	// Error path defaults phase to Failed.
	we := ResultToWire(Result{}, errors.New("boom"))
	if we.Error != "boom" || we.Phase != v1.PhaseFailed {
		t.Errorf("error wire = %+v", we)
	}
}

func TestMaterializeInputs(t *testing.T) {
	ws := t.TempDir()
	leaser := fakeLeaser{vals: map[string]string{"creds": "SECRET-BYTES"}}
	inputs := []v1.RunInputFile{
		{Path: "prompt.txt", Inline: "hello"},
		{Path: "data/blob.bin", InlineBase64: base64.StdEncoding.EncodeToString([]byte{0x00, 0x01, 0x02})},
		{Path: "key.pem", SecretRef: &v1.AuthRef{SecretName: "creds"}},
	}
	if err := MaterializeInputs(context.Background(), ws, inputs, leaser); err != nil {
		t.Fatalf("MaterializeInputs: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "prompt.txt")); string(b) != "hello" {
		t.Errorf("prompt.txt = %q, want hello", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "data/blob.bin")); !bytes.Equal(b, []byte{0, 1, 2}) {
		t.Errorf("blob.bin = %v, want [0 1 2] (base64-decoded, nested dir created)", b)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "key.pem")); string(b) != "SECRET-BYTES" {
		t.Errorf("key.pem = %q, want the leased secret", b)
	}
	if fi, _ := os.Stat(filepath.Join(ws, "prompt.txt")); fi.Mode().Perm() != 0o600 {
		t.Errorf("perms = %v, want 0600", fi.Mode().Perm())
	}
}

func TestMaterializeInputs_NoInputsNoOp(t *testing.T) {
	// No inputs is a no-op even with an empty workspace.
	if err := MaterializeInputs(context.Background(), "", nil, nil); err != nil {
		t.Errorf("no inputs should be a no-op, got %v", err)
	}
}

func TestMaterializeInputs_NoWorkspaceErrors(t *testing.T) {
	err := MaterializeInputs(context.Background(), "  ", []v1.RunInputFile{{Path: "x", Inline: "y"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("err = %v, want a no-workspace error", err)
	}
}

func TestMaterializeInputs_SecretRefWithoutBrokerErrors(t *testing.T) {
	ws := t.TempDir()
	err := MaterializeInputs(context.Background(), ws,
		[]v1.RunInputFile{{Path: "k", SecretRef: &v1.AuthRef{SecretName: "x"}}}, nil)
	if err == nil {
		t.Fatal("secretRef with no broker must fail loud, not write an empty file")
	}
}

func TestMaterializeInputs_TraversalRejected(t *testing.T) {
	ws := t.TempDir()
	err := MaterializeInputs(context.Background(), ws,
		[]v1.RunInputFile{{Path: "../escape.txt", Inline: "x"}}, nil)
	if err == nil {
		t.Fatal("expected traversal to be rejected at write time")
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(ws), "escape.txt")); statErr == nil {
		t.Error("traversal wrote outside the workspace")
	}
}
