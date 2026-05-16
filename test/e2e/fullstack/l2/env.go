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

const (
	ssmDocRunShell = "AWS-RunShellScript"

	sentinelREADY  = "/var/log/l2-bootstrap.READY"
	sentinelFAILED = "/var/log/l2-bootstrap.FAILED"
	bootstrapLog   = "/var/log/l2-bootstrap.log"
)

// ssmAPI is the subset of *ssm.Client that l2Env needs. Lifted to
// an interface so unit tests can swap a fake without standing up
// AWS.
type ssmAPI interface {
	SendCommand(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
}

type l2Env struct {
	ssm        ssmAPI
	instanceID string
	publicIP   string
}

func (c *Cluster) AsEnv() shared.Env {
	return &l2Env{
		ssm:        c.ssmc,
		instanceID: c.InstanceID,
		publicIP:   c.PublicIP,
	}
}

func (e *l2Env) Capabilities() shared.Caps {
	caps := shared.CapKubernetes | shared.CapNetworkEgress |
		shared.CapEBPF | shared.CapKata | shared.CapWebhook |
		shared.CapSPIRE | shared.CapInClusterProbe
	// WG-CLIENT dials the in-cluster wg-hub via NodePort UDP/31820.
	// Without an SG ingress on that port we have no path; scenarios
	// requiring CapWireGuard self-skip in shared.RunAll.
	if e.publicIP != "" {
		caps |= shared.CapWireGuard
	}
	return caps
}

func (e *l2Env) Ring() string { return "l2" }

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
	// 5s tick: cloud-init never completes in under ~5min, so
	// faster polling just burns SSM API calls.
	tick := time.NewTicker(5 * time.Second)
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

func (e *l2Env) Cleanup(_ context.Context) error { return nil }

// Endpoint at L2 returns:
//   - in-cluster Service DNS for scenarios that pass the URL into
//     a probe Pod (PROXY-TCP, PROXY-HTTP) — cluster.local resolves
//     inside the cluster and the L2 driver's CapInClusterProbe
//     branch keeps those scenarios on the RunSpiffeProbe path.
//   - NodePort URL on the instance public IP for scenarios that
//     dial DIRECTLY from the test driver (AGENTRUN's executor.Run),
//     when SG ingress is wired (L2_SECURITY_GROUP_ID + terraform
//     var test_runner_ingress_cidr). Without that, returns ok=false
//     and scenarios self-skip cleanly.
func (e *l2Env) Endpoint(name string) (string, bool) {
	switch name {
	case "fake-gateway-http":
		return "http://fake-gateway.tenant-a.svc.cluster.local:8080", true
	case "fake-gateway-tcp":
		return "fake-gateway.tenant-a.svc.cluster.local:8443", true
	case "fake-llm":
		if e.publicIP != "" {
			return fmt.Sprintf("http://%s:30080", e.publicIP), true
		}
	case "wg-hub":
		// Userspace WireGuard adapter on the driver dials the
		// in-cluster wg-hub via the instance's public IP. UDP
		// NodePort 31820 → pod 51820 (kube-router proxies).
		if e.publicIP != "" {
			return fmt.Sprintf("%s:31820", e.publicIP), true
		}
	}
	return "", false
}

// SPIFFEWorkloadAPI returns the in-Pod socket path. Scenarios that
// use this MUST run via RunSpiffeProbe — same constraint as L1.
func (e *l2Env) SPIFFEWorkloadAPI() string {
	return "unix:///run/spire/agent-sockets/api.sock"
}

func (e *l2Env) RunEBPFProbe(ctx context.Context, scenarios []string, args ...string) ([]shared.ProbeLine, error) {
	pod := fmt.Sprintf("ebpf-probe-%d", time.Now().UnixNano())
	probeArgs := append([]string{"--scenarios=" + strings.Join(scenarios, ",")}, args...)

	// Privileged + hostPID + hostNetwork so we can attach a cgroup
	// program to a path that survives the pod-cgroup namespace and
	// actually filters egress. /sys/fs/cgroup must be writable so
	// link.AttachCgroup can open the cgroup dir; /sys/kernel/btf is
	// the CO-RE source.
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: { name: %s, namespace: tenant-a }
spec:
  restartPolicy: Never
  hostPID: true
  hostNetwork: false
  containers:
    - name: probe
      image: %s/knative-agents/ebpf-probe:%s
      args: %s
      securityContext:
        privileged: true
      volumeMounts:
        - { name: cgroup,  mountPath: /sys/fs/cgroup }
        - { name: bpffs,   mountPath: /sys/fs/bpf }
        - { name: btf,     mountPath: /sys/kernel/btf, readOnly: true }
  volumes:
    - name: cgroup
      hostPath: { path: /sys/fs/cgroup, type: Directory }
    - name: bpffs
      hostPath: { path: /sys/fs/bpf, type: DirectoryOrCreate }
    - name: btf
      hostPath: { path: /sys/kernel/btf, type: Directory }
`, pod, ecrRegistry(), imageTag(), shared.JSONStringList(probeArgs))
	return e.runOneShotProbe(ctx, pod, manifest)
}

func (e *l2Env) RunSpiffeProbe(ctx context.Context, scenarios []string, args ...string) ([]shared.ProbeLine, error) {
	pod := fmt.Sprintf("spiffe-probe-%d", time.Now().UnixNano())
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
`, pod, ecrRegistry(), imageTag(), shared.JSONStringList(probeArgs))
	return e.runOneShotProbe(ctx, pod, manifest)
}

// runOneShotProbe applies a one-shot Pod, waits for it to Succeed,
// returns the parsed OK/FAIL lines from its logs. Used by both
// RunSpiffeProbe and RunEBPFProbe — the only difference between them
// is the manifest.
func (e *l2Env) runOneShotProbe(ctx context.Context, pod, manifest string) ([]shared.ProbeLine, error) {
	if err := e.Apply(ctx, []byte(manifest)); err != nil {
		return nil, fmt.Errorf("apply probe pod: %w", err)
	}
	defer func() {
		_, _ = e.runSSM(ctx, fmt.Sprintf("k0s kubectl -n tenant-a delete pod %s --ignore-not-found", pod),
			30*time.Second)
	}()

	if _, err := e.runSSM(ctx,
		fmt.Sprintf("k0s kubectl -n tenant-a wait --for=jsonpath='{.status.phase}'=Succeeded pod/%s --timeout=60s", pod),
		90*time.Second); err != nil {
		out, _ := e.runSSM(ctx,
			fmt.Sprintf("k0s kubectl -n tenant-a logs %s", pod), 30*time.Second)
		return shared.ParseProbeLines(string(out)),
			fmt.Errorf("probe pod did not succeed: %w\nlogs:\n%s", err, out)
	}

	out, err := e.runSSM(ctx, fmt.Sprintf("k0s kubectl -n tenant-a logs %s", pod), 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("kubectl logs: %w", err)
	}
	return shared.ParseProbeLines(string(out)), nil
}

// runSSM dispatches a shell command via SSM SendCommand, polls
// for completion, returns combined stdout/stderr.
func (e *l2Env) runSSM(ctx context.Context, command string, deadline time.Duration) ([]byte, error) {
	dctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	send, err := e.ssm.SendCommand(dctx, &ssm.SendCommandInput{
		InstanceIds:  []string{e.instanceID},
		DocumentName: aws.String(ssmDocRunShell),
		Parameters:   map[string][]string{"commands": {command}},
	})
	if err != nil {
		return nil, fmt.Errorf("SendCommand: %w", err)
	}
	cmdID := *send.Command.CommandId

	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	for {
		inv, err := e.ssm.GetCommandInvocation(dctx, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(cmdID),
			InstanceId: aws.String(e.instanceID),
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

// quoteShell single-quote-escapes args that contain whitespace,
// quotes, dollar signs, or backslashes — for safe inline shell
// embedding when assembling kubectl args going through SSM.
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
