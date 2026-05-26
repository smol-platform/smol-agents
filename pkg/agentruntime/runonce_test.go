package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if string(res.Output) != "hi from server" {
		t.Errorf("output = %q", res.Output)
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
	w := ResultToWire(Result{Phase: v1.PhaseCompleted, Output: json.RawMessage(`"ok"`), Usage: v1.Usage{Steps: 1}}, nil)
	if w.Phase != v1.PhaseCompleted || string(w.Output) != `"ok"` || w.Error != "" {
		t.Errorf("wire = %+v", w)
	}
	// Error path defaults phase to Failed.
	we := ResultToWire(Result{}, errors.New("boom"))
	if we.Error != "boom" || we.Phase != v1.PhaseFailed {
		t.Errorf("error wire = %+v", we)
	}
}
