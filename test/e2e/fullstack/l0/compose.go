//go:build e2e_l0

package l0

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/stigen/smol-agents/test/e2e/fullstack/shared"
)

// composeEnv is the L0 Env impl. It drives a docker-compose stack
// via the host's `docker compose` CLI (matches the kind-verify
// pattern; avoids adding testcontainers-go to go.mod).
type composeEnv struct {
	projectName string
	composeFile string
	socketDir   string            // host bind for SPIRE workload-API socket
	endpoints   map[string]string // logical name → dial addr
}

// composeUp brings up the stack and returns an Env. The caller MUST
// defer Cleanup. Idempotent — re-running tears down any prior stack
// with the same project name first.
func composeUp(ctx context.Context) (*composeEnv, error) {
	composeFile := repoFile("scripts/e2e/compose-l0.yaml")
	if _, err := os.Stat(composeFile); err != nil {
		return nil, fmt.Errorf("compose file not found at %s: %w", composeFile, err)
	}

	env := &composeEnv{
		projectName: "smol-agents-e2e-l0",
		composeFile: composeFile,
		socketDir:   filepath.Join(os.TempDir(), "smol-agents-e2e-l0-spire-sockets"),
		endpoints: map[string]string{
			"fake-llm":          "http://127.0.0.1:18080",
			"fake-gateway-http": "https://127.0.0.1:18081",
			"fake-gateway-tcp":  "127.0.0.1:18443",
			"wg-hub":            "127.0.0.1:51820",
		},
	}
	if err := os.MkdirAll(env.socketDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir socket dir: %w", err)
	}

	// Tear down any prior stack so we start clean.
	_ = run(ctx, "docker", "compose", "-p", env.projectName, "-f", env.composeFile, "down", "-v")

	// Bring up the stack detached.
	if err := run(ctx, "docker", "compose", "-p", env.projectName, "-f", env.composeFile, "up", "-d", "--wait"); err != nil {
		return nil, fmt.Errorf("compose up: %w", err)
	}

	return env, nil
}

func (e *composeEnv) Capabilities() shared.Caps {
	caps := shared.CapWireGuard
	// CapSPIRE is only advertised when the test process can actually
	// dial the agent socket. On macOS-with-OrbStack the socket file
	// shows up via the bind mount but the kernel can't connect to it
	// (the socket lives in the VM's namespace). On Linux the kernel
	// is shared and the dial works.
	if canDialSocket(e.SPIFFEWorkloadAPI()) {
		caps |= shared.CapSPIRE
	}
	return caps
}

// canDialSocket probe-dials a unix:// URL with a short timeout. We
// use this as the cross-OS gate for CapSPIRE — Linux test hosts
// connect cleanly, macOS-with-OrbStack returns "no such file" even
// though the bind mount makes the file appear.
func canDialSocket(u string) bool {
	const prefix = "unix://"
	if !strings.HasPrefix(u, prefix) {
		return false
	}
	c, err := net.DialTimeout("unix", strings.TrimPrefix(u, prefix), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (e *composeEnv) Ring() string { return "l0" }

// Apply on L0 has no kubernetes apiserver — manifests would be a
// no-op. We return an explicit error so a scenario that needs Apply
// is correctly skipped via capabilities, not silently passing.
func (e *composeEnv) Apply(_ context.Context, _ []byte) error {
	return errors.New("l0: Apply is not applicable (no Kubernetes); gate scenarios with CapKubernetes")
}

// Exec runs a command in a compose service. ExecTarget.Container is
// the service name; Pod and Namespace are ignored at L0.
func (e *composeEnv) Exec(ctx context.Context, target shared.ExecTarget, cmd ...string) ([]byte, error) {
	if target.Container == "" {
		return nil, errors.New("l0.Exec: target.Container (compose service name) is required")
	}
	args := []string{"compose", "-p", e.projectName, "-f", e.composeFile, "exec", "-T", target.Container}
	args = append(args, cmd...)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return out, err
}

// WaitFor polls the predicate at 500ms intervals until it succeeds,
// the deadline fires, or ctx is cancelled.
func (e *composeEnv) WaitFor(ctx context.Context, name string, deadline time.Duration, predicate func(context.Context) bool) error {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		if predicate(dctx) {
			return nil
		}
		select {
		case <-dctx.Done():
			return fmt.Errorf("WaitFor(%s): %w", name, dctx.Err())
		case <-tick.C:
		}
	}
}

// Cleanup tears the stack down. Always runs; idempotent.
func (e *composeEnv) Cleanup(ctx context.Context) error {
	return run(ctx, "docker", "compose", "-p", e.projectName, "-f", e.composeFile, "down", "-v", "--remove-orphans")
}

// Endpoint resolves a logical service name to its host-mapped addr.
func (e *composeEnv) Endpoint(name string) (string, bool) {
	addr, ok := e.endpoints[name]
	return addr, ok
}

// SPIFFEWorkloadAPI returns the unix socket path the test process
// uses to talk to the spire-agent. Bound from the spire-agent
// container into e.socketDir on the host.
func (e *composeEnv) SPIFFEWorkloadAPI() string {
	return "unix://" + filepath.Join(e.socketDir, "api.sock")
}

// RunSpiffeProbe is unsupported at L0 (no Kubernetes cluster).
// SPIFFE scenarios at L0 use the host workload-API directly via
// SPIFFEWorkloadAPI() — gated by CapSPIRE which only flips when the
// host kernel can dial the bind-mounted socket.
func (e *composeEnv) RunSpiffeProbe(_ context.Context, _ []string, _ ...string) ([]shared.ProbeLine, error) {
	return nil, errors.New("l0: in-cluster probe unavailable")
}

func (e *composeEnv) RunEBPFProbe(_ context.Context, _ []string, _ ...string) ([]shared.ProbeLine, error) {
	return nil, errors.New("l0: in-cluster eBPF probe unavailable")
}

// run executes a command, streaming output to stderr on failure.
func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w\nstdout: %s\nstderr: %s",
			name, strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return nil
}

// repoFile resolves a path relative to the project root by walking
// up from this file's directory until go.mod is found.
func repoFile(rel string) string {
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Dir(here)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return rel // fallback: relative to cwd
		}
		dir = parent
	}
}
