package main

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
)

// piBridgeArgs builds the /pi-bridge argv for a pi-mono agent from its harness
// spec (M4.16). Provider/model come from spec.harness.piMono; the base URL +
// provider key reach the bridge via env (PI_BASE_URL / PI_API_KEY), which the
// operator injects — never on the command line.
func piBridgeArgs(agent v1.Agent) []string {
	args := []string{"--addr", "127.0.0.1:8848"}
	if h := agent.Spec.Harness; h != nil && h.PiMono != nil {
		if h.PiMono.Provider != "" {
			args = append(args, "--provider", h.PiMono.Provider)
		}
		if h.PiMono.Model != "" {
			args = append(args, "--model", h.PiMono.Model)
		}
	}
	return args
}

// maybeStartPiBridge starts the in-pod pi-bridge when the mounted Agent is a
// pi-mono harness, waits for its /healthz, and returns a stop func that SIGTERMs
// it (M4.16). A no-op (and a no-op stop) for any other harness/mode, so it is
// safe to call unconditionally from `agent run`.
func maybeStartPiBridge(ctx context.Context, dir string) func() {
	noop := func() {}
	agent, err := loadAgentSpec(filepath.Join(dir, agentruntime.AgentSpecFile))
	if err != nil || agent.Spec.Harness == nil || agent.Spec.Harness.Kind != v1.HarnessPiMono {
		return noop
	}
	cmd := exec.CommandContext(ctx, "/pi-bridge", piBridgeArgs(agent)...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr // bridge logs to stderr; never the key
	if err := cmd.Start(); err != nil {
		return noop
	}
	waitHealthz("http://127.0.0.1:8848/healthz", 30*time.Second)
	return func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			_ = cmd.Wait()
		}
	}
}

// waitHealthz polls url until it returns 200 or the deadline elapses.
func waitHealthz(url string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url) //nolint:gosec // loopback health check
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
}
