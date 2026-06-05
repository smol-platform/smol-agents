package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

func TestWaitForBrokerSocket(t *testing.T) {
	// Absent socket directory => no broker attached => immediate false.
	missing := filepath.Join(t.TempDir(), "nope", "secret-broker.sock")
	start := time.Now()
	if waitForBrokerSocket(missing, 2*time.Second) {
		t.Error("want false when the socket directory is absent")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("absent directory should return immediately, not wait for the timeout")
	}

	// Directory exists and a socket is bound => true. Use a short path: macOS
	// caps unix socket paths (~104 chars) and the default TMPDIR is long.
	dir, err := os.MkdirTemp("/tmp", "bs")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer l.Close()
	if !waitForBrokerSocket(sock, 2*time.Second) {
		t.Error("want true when the socket is bound")
	}

	// Directory exists but no socket ever binds => false after the timeout.
	empty := t.TempDir()
	if waitForBrokerSocket(filepath.Join(empty, "secret-broker.sock"), 300*time.Millisecond) {
		t.Error("want false when the dir exists but no socket binds")
	}
}

func TestClampForTerminationMessage(t *testing.T) {
	fatStep := func(i int32) v1.Step {
		return v1.Step{
			Index: i, Kind: v1.StepObservation,
			ToolCalls: []v1.ToolCallRecord{{
				Tool:      "search",
				Arguments: json.RawMessage(`{"q":"` + strings.Repeat("x", 200) + `"}`),
				Result:    json.RawMessage(`"` + strings.Repeat("y", 400) + `"`),
			}},
		}
	}

	// Small result with a Final step passes through untouched.
	small := agentruntime.RunResult{
		Phase:  "Completed",
		Output: json.RawMessage(`{"answer":42}`),
		Steps:  []v1.Step{{Index: 0, Kind: v1.StepFinal}},
	}
	if got := clampForTerminationMessage(small); len(got.Steps) != 1 || !termMessageFits(got) {
		t.Errorf("small result should keep its steps and fit: steps=%d", len(got.Steps))
	}

	// Large output is truncated to the placeholder (pre-existing behavior).
	bigOut := agentruntime.RunResult{Phase: "Completed", Output: json.RawMessage(`"` + strings.Repeat("z", 4096) + `"`)}
	if got := clampForTerminationMessage(bigOut); string(got.Output) != `"<truncated; see pod logs>"` {
		t.Errorf("large output not truncated: %s", got.Output)
	}

	// Many fat steps must still fit the cap and stay valid JSON — never overflow,
	// and never lose the verdict fields (phase/output).
	steps := make([]v1.Step, 50)
	for i := range steps {
		steps[i] = fatStep(int32(i))
	}
	fat := agentruntime.RunResult{
		Phase:  "Completed",
		Output: json.RawMessage(`{"answer":42}`),
		Steps:  steps,
		Usage:  v1.Usage{Steps: 50},
		Trace:  &v1.TraceSummary{StepCount: 50, ToolCallCount: 100},
	}
	got := clampForTerminationMessage(fat)
	b := marshalRunResult(got)
	if len(b) > terminationMessageBudget {
		t.Errorf("clamped message = %d bytes, exceeds budget %d", len(b), terminationMessageBudget)
	}
	if !json.Valid(b) {
		t.Errorf("clamped message must be valid JSON: %s", b)
	}
	if got.Phase != "Completed" || string(got.Output) != `{"answer":42}` {
		t.Errorf("clamp must preserve phase/output: %+v", got)
	}
	// M2.3: when steps are dropped, the trace must survive + mark the loss honestly.
	if len(got.Steps) != 0 {
		t.Errorf("fat steps should be dropped, got %d", len(got.Steps))
	}
	if got.Trace == nil || !got.Trace.Truncated || got.Trace.DroppedBytes == 0 || got.Trace.StepCount != 50 {
		t.Errorf("dropped trace must stay honest: %+v", got.Trace)
	}
}
