//go:build e2e_l1

package l1

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

	"github.com/stigen/knative-agents/test/e2e/fullstack/shared"
)

// kindEnv is the L1 Env impl. It runs kind-verify.sh for setup
// (matches what's already validated in CI), then wraps kubectl for
// Apply/Exec.
type kindEnv struct {
	cluster   string
	context   string
	endpoints map[string]string

	// scriptPath points at scripts/kind-verify.sh. The L1 driver
	// reuses that script's bring-up because it already creates the
	// cluster, builds + loads the operator image, applies CRDs +
	// RBAC + manager via the kind overlay, and pre-creates the
	// `<agent>-agent` ServiceAccount.
	scriptPath string
}

// kindUp brings the cluster up. Idempotent — kind-verify.sh reuses
// an existing cluster if found.
func kindUp(ctx context.Context) (*kindEnv, error) {
	script := repoFile("scripts/kind-verify.sh")
	if _, err := os.Stat(script); err != nil {
		return nil, fmt.Errorf("kind-verify.sh not found at %s: %w", script, err)
	}

	cluster := os.Getenv("CLUSTER")
	if cluster == "" {
		cluster = "knative-agents-kind"
	}

	env := &kindEnv{
		cluster:    cluster,
		context:    "kind-" + cluster,
		scriptPath: script,
		// L1 doesn't currently deploy fake-llm / fake-gateway /
		// wg-hub in-cluster. Scenarios that need them self-skip via
		// `env.Endpoint(name)` returning false. Wiring those in is
		// a follow-up (T-2.x) — at that point they become k8s
		// Service DNS names like fake-llm.tenant-a.svc.cluster.local.
		endpoints: map[string]string{},
	}

	cmd := exec.CommandContext(ctx, "bash", script)
	cmd.Env = append(os.Environ(), "CLUSTER="+cluster)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kind-verify.sh: %w\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}
	return env, nil
}

func (e *kindEnv) Capabilities() shared.Caps {
	caps := shared.CapKubernetes | shared.CapNetworkEgress
	// CapEBPF advertised when the kind nodes expose bpf() — they do
	// on Linux + OrbStack VMs. We can probe by listing the
	// ebpf-loader DaemonSet's Pod logs but that adds setup; for now
	// trust the kind-on-Linux invariant and advertise.
	caps |= shared.CapEBPF
	// CapSPIRE only when an in-cluster test runner can reach the
	// workload-API socket. Like L0 on macOS, the host can't dial
	// sockets in the Linux VM; defer SPIRE-requiring tests to a
	// future in-cluster test-driver Pod (T-2.x follow-up).
	if e.canReachSPIREInCluster() {
		caps |= shared.CapSPIRE
	}
	return caps
}

func (e *kindEnv) Ring() string { return "l1" }

// Apply kubectl-applies the manifest against the kind cluster.
func (e *kindEnv) Apply(ctx context.Context, manifest []byte) error {
	cmd := exec.CommandContext(ctx, "kubectl", "--context", e.context, "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl apply: %w: %s", err, out)
	}
	return nil
}

// Exec runs a command in a Pod via kubectl exec.
func (e *kindEnv) Exec(ctx context.Context, target shared.ExecTarget, cmd ...string) ([]byte, error) {
	if target.Pod == "" {
		return nil, errors.New("l1.Exec: target.Pod is required")
	}
	args := []string{"--context", e.context, "exec", "-n", target.Namespace}
	if target.Container != "" {
		args = append(args, "-c", target.Container)
	}
	args = append(args, target.Pod, "--")
	args = append(args, cmd...)
	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	return out, err
}

func (e *kindEnv) WaitFor(ctx context.Context, name string, deadline time.Duration, predicate func(context.Context) bool) error {
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

// Cleanup deletes the kind cluster. Disabled by default to keep
// inner-loop dev fast; set L1_TEARDOWN=1 to opt in.
func (e *kindEnv) Cleanup(ctx context.Context) error {
	if os.Getenv("L1_TEARDOWN") != "1" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "kind", "delete", "cluster", "--name", e.cluster)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kind delete: %w: %s", err, out)
	}
	return nil
}

func (e *kindEnv) Endpoint(logical string) (string, bool) {
	addr, ok := e.endpoints[logical]
	return addr, ok
}

// SPIFFEWorkloadAPI returns the in-Pod socket path. Scenarios that
// use this MUST run via Exec, not from the host (the host kernel
// can't dial sockets that live inside a Pod on the kind node).
func (e *kindEnv) SPIFFEWorkloadAPI() string {
	return "unix:///run/spire/agent-sockets/api.sock"
}

// canReachSPIREInCluster probes whether SPIRE is even installed in
// the cluster. The current scripts/kind-verify.sh doesn't deploy
// SPIRE — that's a separate enhancement (T-2.x). For now we conclude
// CapSPIRE is unavailable at L1.
func (e *kindEnv) canReachSPIREInCluster() bool {
	cmd := exec.Command("kubectl", "--context", e.context, "get", "namespace", "spire-system",
		"--ignore-not-found", "-o", "name")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// ----------------------------- helpers -----------------------------

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
			return rel
		}
		dir = parent
	}
}

// kindAvailable reports whether the kind CLI + a working docker
// daemon are present. L1 self-skips when not.
func kindAvailable() bool {
	if _, err := exec.LookPath("kind"); err != nil {
		return false
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		return false
	}
	cmd := exec.Command("docker", "info")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// avoid "imported and not used" for net when the only consumer is a
// future SPIRE probe sitting under build constraints.
var _ = net.Dial
