package shared

import (
	"context"
	"testing"
	"time"
)

func TestAll_HasEveryScenario(t *testing.T) {
	got := All()
	if len(got) < 10 {
		t.Errorf("All() returned %d scenarios, expected at least 10", len(got))
	}
	seen := map[string]bool{}
	for _, s := range got {
		if s.ID == "" {
			t.Errorf("scenario %q has empty ID", s.Name)
		}
		if s.Run == nil {
			t.Errorf("scenario %q has nil Run", s.ID)
		}
		if seen[s.ID] {
			t.Errorf("duplicate scenario ID %q", s.ID)
		}
		seen[s.ID] = true
	}
}

func TestRunAll_SkipsWhenCapsMissing(t *testing.T) {
	env := &fakeEnv{caps: 0}
	called := false
	scenarios := []Scenario{{
		ID:       "R-TEST-1",
		Name:     "needs-kata",
		Requires: CapKata,
		Run:      func(_ *testing.T, _ Env) { called = true },
	}}
	t.Run("inner", func(t *testing.T) { RunAll(t, env, scenarios) })
	if called {
		t.Error("Run should have been skipped, not invoked")
	}
}

func TestRunAll_RunsWhenCapsPresent(t *testing.T) {
	env := &fakeEnv{caps: CapSPIRE | CapWireGuard}
	called := false
	scenarios := []Scenario{{
		ID:       "R-TEST-2",
		Name:     "needs-spire",
		Requires: CapSPIRE,
		Run:      func(_ *testing.T, _ Env) { called = true },
	}}
	t.Run("inner", func(t *testing.T) { RunAll(t, env, scenarios) })
	if !called {
		t.Error("Run should have been invoked")
	}
}

// fakeEnv is a no-op Env satisfying the full interface for runner tests.
type fakeEnv struct{ caps Caps }

func (f *fakeEnv) Capabilities() Caps                                                { return f.caps }
func (f *fakeEnv) Ring() string                                                      { return "fake" }
func (f *fakeEnv) Apply(_ context.Context, _ []byte) error                           { return nil }
func (f *fakeEnv) Exec(_ context.Context, _ ExecTarget, _ ...string) ([]byte, error) { return nil, nil }
func (f *fakeEnv) WaitFor(_ context.Context, _ string, _ time.Duration, _ func(context.Context) bool) error {
	return nil
}
func (f *fakeEnv) Cleanup(_ context.Context) error  { return nil }
func (f *fakeEnv) Endpoint(_ string) (string, bool) { return "", false }
func (f *fakeEnv) SPIFFEWorkloadAPI() string        { return "" }
func (f *fakeEnv) RunSpiffeProbe(_ context.Context, _ []string, _ ...string) ([]ProbeLine, error) {
	return nil, nil
}
func (f *fakeEnv) RunEBPFProbe(_ context.Context, _ []string, _ ...string) ([]ProbeLine, error) {
	return nil, nil
}
