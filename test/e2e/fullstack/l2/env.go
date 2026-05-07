//go:build e2e_l2

package l2

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/stigen/knative-agents/test/e2e/fullstack/shared"
)

// l2Env satisfies shared.Env by routing every operation through SSM
// SendCommand against the bootstrapped EC2 instance. The instance's
// SSM agent runs the requested shell snippets as root, with kubectl
// already configured by the cloud-init template via /var/lib/k0s/
// pki/admin.conf.
//
// Trade-off vs the L1 kubectl-from-host pattern: SSM has ~1-2s
// per-call latency (round-trip through AWS API), so scenario time
// dominates by SSM throughput rather than test logic. Acceptable
// for the smaller L2 scenario set.
// ssmAPI is the subset of *ssm.Client that l2Env uses. Captured as
// an interface so tests can plug a fake without standing up AWS.
type ssmAPI interface {
	SendCommand(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}

type l2Env struct {
	cluster *Cluster
	// ssm overrides cluster.ssmc when non-nil. Production code
	// leaves this nil; tests inject a fakeSSM. The accessor below
	// resolves which to use.
	ssm ssmAPI
}

// AsEnv wraps a Cluster as an Env. Used by TestL2 to call
// shared.RunAll(t, env, shared.All()) once the instance is up.
func (c *Cluster) AsEnv() shared.Env { return &l2Env{cluster: c} }

// ssmClient resolves the SSM client to use — the test override if
// set, otherwise the cluster's real client.
func (e *l2Env) ssmClient() ssmAPI {
	if e.ssm != nil {
		return e.ssm
	}
	return e.cluster.ssmc
}

// instanceID resolves the instance to target. cluster may be nil in
// unit tests; in that case the test must set ssm + a fixed ID via
// the testing-only newTestEnv helper.
func (e *l2Env) instanceID() string {
	if e.cluster != nil {
		return e.cluster.InstanceID
	}
	return "i-test"
}

func (e *l2Env) Capabilities() shared.Caps {
	return shared.CapKubernetes | shared.CapNetworkEgress |
		shared.CapEBPF | shared.CapKata | shared.CapWebhook |
		shared.CapSPIRE | shared.CapInClusterProbe
}

func (e *l2Env) Ring() string { return "l2" }

// Apply pipes the manifest through `kubectl apply -f -` on the
// instance via SSM. SSM has a 10KB stdin limit; the cloud-init
// template intentionally keeps individual manifests small.
func (e *l2Env) Apply(ctx context.Context, manifest []byte) error {
	cmd := fmt.Sprintf(`set -e
cat <<'EOFMANIFEST' | k0s kubectl apply -f -
%s
EOFMANIFEST
`, manifest)
	out, err := e.runSSM(ctx, cmd, 60*time.Second)
	if err != nil {
		return fmt.Errorf("kubectl apply: %w: %s", err, out)
	}
	return nil
}

// Exec runs a kubectl-routed command on the instance. With
// target.Pod=="" runs `k0s kubectl <args>` directly. With Pod set,
// runs `k0s kubectl exec -n <ns> [-c <ctr>] <pod> -- <cmd>`.
func (e *l2Env) Exec(ctx context.Context, target shared.ExecTarget, cmd ...string) ([]byte, error) {
	var args []string
	if target.Pod == "" {
		args = append([]string{"k0s", "kubectl"}, cmd...)
	} else {
		args = []string{"k0s", "kubectl", "exec", "-n", target.Namespace}
		if target.Container != "" {
			args = append(args, "-c", target.Container)
		}
		args = append(args, target.Pod, "--")
		args = append(args, cmd...)
	}
	return e.runSSM(ctx, strings.Join(quoteShell(args), " "), 60*time.Second)
}

func (e *l2Env) WaitFor(ctx context.Context, name string, deadline time.Duration, predicate func(context.Context) bool) error {
	tick := time.NewTicker(2 * time.Second)
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

// Cleanup is a no-op at the Env layer; the test's t.Cleanup hook
// calls Cluster.Teardown() which terminates the instance.
func (e *l2Env) Cleanup(_ context.Context) error { return nil }

// Endpoint at L2 returns the in-cluster Service DNS for in-cluster
// scenarios (probe Pod via RunSpiffeProbe). Scenarios that try to
// dial directly from the test driver self-skip — we don't bridge
// host ↔ EC2 instance via port-forward at L2; SSM is the contract.
func (e *l2Env) Endpoint(name string) (string, bool) {
	switch name {
	case "fake-llm":
		return "http://fake-llm.tenant-a.svc.cluster.local:8080", true
	case "fake-gateway-http":
		return "http://fake-gateway.tenant-a.svc.cluster.local:8080", true
	case "fake-gateway-tcp":
		return "fake-gateway.tenant-a.svc.cluster.local:8443", true
	}
	return "", false
}

// SPIFFEWorkloadAPI returns the in-Pod socket path. Scenarios that
// use this MUST run via RunSpiffeProbe — same constraint as L1.
func (e *l2Env) SPIFFEWorkloadAPI() string {
	return "unix:///run/spire/agent-sockets/api.sock"
}

// RunSpiffeProbe at L2 launches a one-shot Pod (same shape as L1's
// implementation) but via SSM-driven kubectl run. Output is
// captured from `kubectl logs`.
func (e *l2Env) RunSpiffeProbe(ctx context.Context, scenarios []string, args ...string) ([]shared.ProbeLine, error) {
	pod := "spiffe-probe-" + fmt.Sprintf("%d", time.Now().UnixNano())
	probeArgs := append([]string{"--scenarios=" + strings.Join(scenarios, ",")}, args...)

	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: { name: %s, namespace: tenant-a }
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: %s/knative-agents/spiffe-probe:%s
      args: %s
      volumeMounts:
        - { name: sockets, mountPath: /run/spire/agent-sockets }
  volumes:
    - name: sockets
      hostPath: { path: /run/spire/agent-sockets, type: DirectoryOrCreate }
`, pod, ecrRegistry(), imageTag(), jsonStringList(probeArgs))

	if err := e.Apply(ctx, []byte(manifest)); err != nil {
		return nil, fmt.Errorf("apply probe pod: %w", err)
	}
	defer func() {
		_, _ = e.runSSM(ctx, fmt.Sprintf("k0s kubectl -n tenant-a delete pod %s --ignore-not-found", pod),
			30*time.Second)
	}()

	// Wait for completion via kubectl wait through SSM.
	_, err := e.runSSM(ctx,
		fmt.Sprintf("k0s kubectl -n tenant-a wait --for=jsonpath='{.status.phase}'=Succeeded pod/%s --timeout=30s", pod),
		60*time.Second)
	if err != nil {
		out, _ := e.runSSM(ctx,
			fmt.Sprintf("k0s kubectl -n tenant-a logs %s", pod), 30*time.Second)
		return parseProbeLines(string(out)),
			fmt.Errorf("probe pod did not succeed: %w\nlogs:\n%s", err, out)
	}

	out, err := e.runSSM(ctx, fmt.Sprintf("k0s kubectl -n tenant-a logs %s", pod), 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("kubectl logs: %w", err)
	}
	return parseProbeLines(string(out)), nil
}

// ----------------------------- internals ---------------------------

// runSSM dispatches a shell command via SSM SendCommand to the
// instance, polls for completion, returns combined stdout/stderr.
func (e *l2Env) runSSM(ctx context.Context, command string, deadline time.Duration) ([]byte, error) {
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	send, err := e.ssmClient().SendCommand(dctx, &ssm.SendCommandInput{
		InstanceIds:  []string{e.instanceID()},
		DocumentName: aws.String("AWS-RunShellScript"),
		Parameters: map[string][]string{
			"commands": {command},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("SendCommand: %w", err)
	}
	cmdID := *send.Command.CommandId

	// Poll for completion.
	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	for {
		inv, err := e.ssmClient().GetCommandInvocation(dctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(cmdID),
			InstanceId: aws.String(e.instanceID()),
		})
		if err == nil && inv.Status != ssmtypes.CommandInvocationStatusInProgress &&
			inv.Status != ssmtypes.CommandInvocationStatusPending {
			out := []byte("")
			if inv.StandardOutputContent != nil {
				out = []byte(*inv.StandardOutputContent)
			}
			if inv.Status != ssmtypes.CommandInvocationStatusSuccess {
				stderr := ""
				if inv.StandardErrorContent != nil {
					stderr = *inv.StandardErrorContent
				}
				return out, fmt.Errorf("ssm command %s status=%s\nstderr: %s",
					cmdID, inv.Status, stderr)
			}
			return out, nil
		}
		select {
		case <-dctx.Done():
			return nil, fmt.Errorf("ssm command %s timed out: %w", cmdID, dctx.Err())
		case <-tick.C:
		}
	}
}

// parseProbeLines mirrors L1's; kept local to avoid importing l1.
func parseProbeLines(s string) []shared.ProbeLine {
	var out []shared.ProbeLine
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "OK "):
			rest := strings.TrimPrefix(line, "OK ")
			scen, detail, _ := strings.Cut(rest, " ")
			out = append(out, shared.ProbeLine{OK: true, Scenario: scen, Detail: detail})
		case strings.HasPrefix(line, "FAIL "):
			rest := strings.TrimPrefix(line, "FAIL ")
			scen, detail, _ := strings.Cut(rest, " ")
			out = append(out, shared.ProbeLine{OK: false, Scenario: scen, Detail: detail})
		}
	}
	return out
}

func jsonStringList(items []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(s, `"`, `\"`))
		b.WriteByte('"')
	}
	b.WriteByte(']')
	return b.String()
}

// quoteShell returns each arg single-quote escaped for safe inline
// shell embedding. Used to assemble kubectl args going through SSM.
func quoteShell(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t'\"$\\") {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			out[i] = a
		}
	}
	return out
}

func ecrRegistry() string { return envOrDefault("L2_ECR_REGISTRY", "") }
func imageTag() string    { return envOrDefault("L2_IMAGE_TAG", "dev") }
