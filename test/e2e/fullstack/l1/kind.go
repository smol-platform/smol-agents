//go:build e2e_l1

package l1

import (
	"bytes"
	"context"
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

	// scriptPath points at scripts/kind-verify.sh.
	scriptPath string

	// portForwards holds the cancel-funcs for `kubectl port-forward`
	// goroutines; cleanup teardown stops them.
	portForwards []func()
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
		// Endpoints populate after deployFakes() lands the in-cluster
		// services; we keep this nil-on-construct so a Failed bring-
		// up doesn't accidentally claim endpoints exist.
		endpoints: nil,
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

	// Build + load the fake services + apply their manifests so
	// scenarios that need fake-llm / fake-gateway in-cluster have
	// targets to dial.
	if err := env.deployFakes(ctx); err != nil {
		return nil, fmt.Errorf("deploy fakes: %w", err)
	}
	// Port-forward in-cluster services to host ports so the test
	// driver (running on the host, outside the cluster) can dial
	// them. Without this, the cluster-internal DNS names don't
	// resolve from the host. Each call also waits for the local
	// listener to actually accept TCP connections so scenarios
	// don't race the forwarder's startup.
	pfs := []struct {
		logical, svc, ns  string
		hostPort, svcPort int
	}{
		{"fake-llm", "fake-llm", "tenant-a", 18090, 8080},
		{"fake-gateway-http", "fake-gateway", "tenant-a", 18091, 8080},
		{"fake-gateway-tcp", "fake-gateway", "tenant-a", 18092, 8443},
	}
	env.endpoints = map[string]string{}
	for _, p := range pfs {
		stop, err := env.portForward(ctx, p.ns, "svc/"+p.svc, p.hostPort, p.svcPort)
		if err != nil {
			env.stopPortForwards()
			return nil, fmt.Errorf("port-forward %s: %w", p.logical, err)
		}
		env.portForwards = append(env.portForwards, stop)
		switch p.logical {
		case "fake-gateway-tcp":
			env.endpoints[p.logical] = fmt.Sprintf("127.0.0.1:%d", p.hostPort)
		default:
			env.endpoints[p.logical] = fmt.Sprintf("http://127.0.0.1:%d", p.hostPort)
		}
	}

	return env, nil
}

// portForward starts `kubectl port-forward` in the background and
// returns a stop function. Blocks until the host port accepts
// connections, with a generous timeout to absorb kubectl warm-up.
func (e *kindEnv) portForward(ctx context.Context, ns, target string, hostPort, svcPort int) (func(), error) {
	pfCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(pfCtx, "kubectl", "--context", e.context,
		"-n", ns, "port-forward", target,
		fmt.Sprintf("%d:%d", hostPort, svcPort))
	// Discard stdout/stderr — kubectl port-forward is chatty and we
	// only care about whether the local listener comes up.
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	stop := func() {
		cancel()
		_ = cmd.Wait()
	}

	// Poll the host port until it accepts.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return stop, nil
		}
		select {
		case <-ctx.Done():
			stop()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	stop()
	return nil, fmt.Errorf("port-forward 127.0.0.1:%d never came up", hostPort)
}

func (e *kindEnv) stopPortForwards() {
	for _, stop := range e.portForwards {
		stop()
	}
	e.portForwards = nil
}

// deployFakes builds the fake-llm + spire-shell + spiffe-probe
// images, loads them into the kind cluster, and kubectl-applies all
// e2e manifests (fakes + SPIRE). Idempotent.
func (e *kindEnv) deployFakes(ctx context.Context) error {
	root := repoFile("")
	// 1. Build images we ship locally.
	for _, img := range []struct{ tag, dockerfile, ctx string }{
		{"knative-agents/fake-llm:dev", "deploy/docker/fake-llm.Dockerfile", root},
		{"knative-agents/spiffe-probe:dev", "deploy/docker/spiffe-probe.Dockerfile", root},
		{"knative-agents/spire-shell:dev", "scripts/e2e/spire/Dockerfile.spire-shell", filepath.Join(root, "scripts/e2e/spire")},
	} {
		if err := runCmd(ctx, "docker", "build",
			"-f", filepath.Join(root, img.dockerfile),
			"-t", img.tag, img.ctx); err != nil {
			return fmt.Errorf("docker build %s: %w", img.tag, err)
		}
		if err := runCmd(ctx, "kind", "load", "docker-image",
			img.tag, "--name", e.cluster); err != nil {
			return fmt.Errorf("kind load %s: %w", img.tag, err)
		}
	}
	// 2. Apply fake services.
	manifests := filepath.Join(root, "test/e2e/manifests")
	if err := runCmd(ctx, "kubectl", "--context", e.context,
		"apply", "-k", manifests); err != nil {
		return fmt.Errorf("kubectl apply fakes: %w", err)
	}
	// 3. Apply SPIRE.
	if err := runCmd(ctx, "kubectl", "--context", e.context,
		"apply", "-k", filepath.Join(manifests, "spire")); err != nil {
		return fmt.Errorf("kubectl apply spire: %w", err)
	}
	// 3b. Install cert-manager (webhook overlay needs it). Skip if
	// already there.
	out, _ := exec.CommandContext(ctx, "kubectl", "--context", e.context,
		"get", "ns", "cert-manager", "--ignore-not-found", "-o", "name").Output()
	if strings.TrimSpace(string(out)) == "" {
		_ = runCmd(ctx, "kubectl", "--context", e.context, "apply",
			"-f", "https://github.com/cert-manager/cert-manager/releases/download/v1.16.0/cert-manager.yaml")
		_ = runCmd(ctx, "kubectl", "--context", e.context, "-n", "cert-manager",
			"wait", "--for=condition=available", "deployment", "--all",
			"--timeout=120s")
	}
	// 3c. Apply the kind-webhook overlay — adds the operator's
	// validating webhook + cert-manager Issuer/Certificate. Replaces
	// the no-webhook deployment from kind-verify.sh's overlay.
	if err := runCmd(ctx, "kubectl", "--context", e.context,
		"apply", "-k", filepath.Join(root, "operator/config/kind-webhook")); err != nil {
		return fmt.Errorf("kubectl apply webhook overlay: %w", err)
	}
	// 3d. Wait for the rolled-out operator pod to be Ready.
	if err := runCmd(ctx, "kubectl", "--context", e.context,
		"-n", "knative-agents-system", "rollout", "status",
		"deployment/knative-agents-operator", "--timeout=120s"); err != nil {
		return fmt.Errorf("wait operator rollout: %w", err)
	}
	// 4. Wait for fakes ready.
	if err := runCmd(ctx, "kubectl", "--context", e.context,
		"-n", "tenant-a", "wait", "--for=condition=available",
		"deployment/fake-llm", "deployment/fake-gateway",
		"--timeout=120s"); err != nil {
		return fmt.Errorf("wait fakes ready: %w", err)
	}
	// 5. Wait for SPIRE server (best-effort — agent + bootstrap job
	// take longer; we don't block on them since CapSPIRE is checked
	// dynamically via canReachSPIREInCluster).
	_ = runCmd(ctx, "kubectl", "--context", e.context,
		"-n", "spire-system", "wait", "--for=condition=ready", "pod",
		"-l", "app=spire-server", "--timeout=60s")
	return nil
}

func runCmd(ctx context.Context, name string, args ...string) error {
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

func (e *kindEnv) Capabilities() shared.Caps {
	caps := shared.CapKubernetes | shared.CapNetworkEgress | shared.CapInClusterProbe
	caps |= shared.CapEBPF
	if e.canReachSPIREInCluster() {
		caps |= shared.CapSPIRE
	}
	if e.hasWebhook() {
		caps |= shared.CapWebhook
	}
	return caps
}

// hasWebhook reports whether the operator's ValidatingWebhook is
// installed. Used to gate the S-WEBHOOK scenario.
func (e *kindEnv) hasWebhook() bool {
	out, err := exec.Command("kubectl", "--context", e.context, "get",
		"validatingwebhookconfiguration", "knative-agents-operator-validating",
		"--ignore-not-found", "-o", "name").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
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

// Exec runs a command. With target.Pod empty, runs kubectl directly
// (e.g. `Exec(ctx, ExecTarget{}, "get", "ns")` → `kubectl get ns`).
// With target.Pod set, runs `kubectl exec -n <ns> [-c <ctr>] <pod>
// -- <cmd>` to execute inside that Pod.
func (e *kindEnv) Exec(ctx context.Context, target shared.ExecTarget, cmd ...string) ([]byte, error) {
	args := []string{"--context", e.context}
	if target.Pod == "" {
		args = append(args, cmd...)
	} else {
		args = append(args, "exec", "-n", target.Namespace)
		if target.Container != "" {
			args = append(args, "-c", target.Container)
		}
		args = append(args, target.Pod, "--")
		args = append(args, cmd...)
	}
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

// Cleanup stops port-forwards (always) and optionally deletes the
// cluster. Cluster delete is opt-in via L1_TEARDOWN=1 to keep the
// inner-loop fast; port-forwards always stop because they hold host
// ports + child processes.
func (e *kindEnv) Cleanup(ctx context.Context) error {
	e.stopPortForwards()
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

// canReachSPIREInCluster probes whether SPIRE is installed AND the
// spire-server StatefulSet is ready. We require both because just
// having the namespace isn't sufficient — workload entries must be
// registered (which the bootstrap sidecar does) for any probe to
// fetch an SVID.
func (e *kindEnv) canReachSPIREInCluster() bool {
	out, err := exec.Command("kubectl", "--context", e.context,
		"-n", "spire-system", "get", "pod", "spire-server-0",
		"-o", "jsonpath={.status.phase}").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "Running"
}

// RunSpiffeProbe applies a one-shot Pod running cmd/spiffe-probe
// with the given scenario list, waits for completion, and returns
// the parsed lines. Used by SPIFFE-requiring scenarios at L1
// because the host can't dial the in-Pod workload-API socket.
func (e *kindEnv) RunSpiffeProbe(ctx context.Context, scenarios []string, args ...string) ([]shared.ProbeLine, error) {
	pod := "spiffe-probe-" + fmt.Sprintf("%d", time.Now().UnixNano())
	probeArgs := append([]string{"--scenarios=" + strings.Join(scenarios, ",")}, args...)

	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: { name: %s, namespace: tenant-a }
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: knative-agents/spiffe-probe:dev
      imagePullPolicy: Never
      args: %s
      volumeMounts:
        - { name: sockets, mountPath: /run/spire/agent-sockets }
  volumes:
    - name: sockets
      hostPath: { path: /run/spire/agent-sockets, type: DirectoryOrCreate }
`, pod, shared.JSONStringList(probeArgs))

	if err := e.Apply(ctx, []byte(manifest)); err != nil {
		return nil, fmt.Errorf("apply probe pod: %w", err)
	}
	defer func() {
		_ = exec.Command("kubectl", "--context", e.context,
			"-n", "tenant-a", "delete", "pod", pod, "--ignore-not-found").Run()
	}()

	// Wait for completion.
	wait := exec.CommandContext(ctx, "kubectl", "--context", e.context,
		"-n", "tenant-a", "wait", "--for=jsonpath={.status.phase}=Succeeded",
		"pod/"+pod, "--timeout=30s")
	if err := wait.Run(); err != nil {
		// Probe failed; still grab logs to surface why.
		out, _ := exec.Command("kubectl", "--context", e.context,
			"-n", "tenant-a", "logs", pod).CombinedOutput()
		return shared.ParseProbeLines(string(out)), fmt.Errorf("probe pod did not succeed: %w\nlogs:\n%s", err, out)
	}

	out, err := exec.Command("kubectl", "--context", e.context,
		"-n", "tenant-a", "logs", pod).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl logs: %w", err)
	}
	return shared.ParseProbeLines(string(out)), nil
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
