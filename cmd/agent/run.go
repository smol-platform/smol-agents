package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/smol-platform/smol-agents/pkg/agentfs"
	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/agentruntime/invokers"
	"github.com/smol-platform/smol-agents/pkg/agentruntime/openaillm"
	"github.com/smol-platform/smol-agents/pkg/secrets"
)

// secretLeaser adapts pkg/secrets.Client to agentruntime.SecretLeaser.
type secretLeaser struct{ c *secrets.Client }

func (l secretLeaser) LeaseSecret(ctx context.Context, name string, ttl time.Duration) ([]byte, error) {
	lease, err := l.c.Lease(ctx, name, ttl)
	if err != nil {
		return nil, err
	}
	return lease.Value, nil
}

// runAgentRun is the `agent run` subcommand, executed inside an AgentRun pod:
// load the mounted Agent + run spec, execute one bounded run, and emit the
// RunResult to the k8s termination message (the controller's primary signal)
// and stdout (logs). Returns the process exit code.
func runAgentRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dir := fs.String("dir", "/etc/smol-agents/run", "directory with agent.json + run.json")
	socket := fs.String("secret-socket", "/run/secret-broker/secret-broker.sock", "secret broker UDS")
	termLog := fs.String("termination-log", "/dev/termination-log", "k8s termination message path")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Wire the broker leaser when the broker sidecar is attached. The sidecar
	// starts concurrently with this container, so the socket can lag our
	// startup by a few seconds — poll for it. The executor errors clearly if a
	// secretRef is declared but no broker ever appears.
	var leaser agentruntime.SecretLeaser
	if waitForBrokerSocket(*socket, 60*time.Second) {
		leaser = secretLeaser{c: secrets.NewClient(*socket)}
	}

	// Register the loop-mode tool invokers (HTTP + MCP; M2.12/M2.14). The tool
	// catalog itself is loaded from tools.json by RunOnce. WireAgentInvoker adds
	// the kind=agent (A2A) invoker when the pod has in-cluster API access (M3 A1);
	// WireTaskInvoker / WireTeammateInvoker / WireTeamBusInvoker add the kind=task
	// (team shared task list, P1), kind=teammate (peer mailbox, P2), and
	// kind=teambus (message bus, P5) invokers when the pod carries a team context.
	// WireFanoutInvoker adds kind=fanout (Send map-reduce) under the A2A in-cluster
	// gate. Without each, that kind stays fail-closed.
	toolInvokers := invokers.WireFanoutInvoker(invokers.WireTeamBusInvoker(invokers.WireTeammateInvoker(invokers.WireTaskInvoker(invokers.WireAgentInvoker(invokers.Default(leaser, nil))))))

	// pi-mono (M4.16): start the in-pod pi-bridge before the run so the harness's
	// HTTP call to 127.0.0.1:8848 lands; SIGTERM it on exit. No-op for other kinds.
	stopBridge := maybeStartPiBridge(ctx, *dir)
	defer stopBridge()

	llm := buildLoopLLM(ctx, *dir, leaser)

	// rv3.1 S5: a generator-verifier team coordinator (TEAM_NAME set, no
	// TEAM_MEMBER) drives the convergence loop instead of a plain plan-act-observe
	// loop. Every non-coordinator run falls through to RunOnce unchanged.
	var wire agentruntime.RunResult
	failed := false
	if cr, handled, cerr := maybeRunCoordinator(ctx, *dir, toolInvokers, llm); handled {
		if cerr != nil {
			wire = agentruntime.RunResult{Phase: v1.PhaseFailed, Error: cerr.Error(), TerminationReason: "CoordinatorError"}
			failed = true
		} else {
			wire = cr
		}
	} else {
		res, runErr := agentruntime.RunOnce(ctx, *dir, leaser, llm,
			agentruntime.WithInvokers(toolInvokers))
		wire = agentruntime.ResultToWire(res, runErr)
		failed = runErr != nil
	}

	// Full result to stdout for log-based debugging.
	os.Stdout.Write(marshalRunResult(wire))
	os.Stdout.Write([]byte("\n"))

	// M2.9: when the result won't fit the termination cap, park the FULL RunResult
	// in the per-tenant overflow sink (if configured) and stamp Trace.OverflowRef
	// BEFORE clamping, so the trimmed message points at the recoverable detail. No
	// sink configured → the full result lives in the stdout logs only.
	if !termMessageFits(wire) {
		if sink := overflowSinkFromEnv(ctx); sink != nil {
			wire = stampOverflowTrace(wire, sink, os.Getenv("POD_NAMESPACE"), os.Getenv("RUN_NAME"))
		}
	}

	// The termination message is the controller's primary signal and must stay
	// valid JSON under the kubelet's ~4KiB cap. clampForTerminationMessage trims
	// it to fit so a large run can't overflow the cap and corrupt the JSON —
	// which would fail the controller's fold and silently zero the run. The
	// full, untrimmed result is in the stdout logs written above.
	_ = os.WriteFile(*termLog, marshalRunResult(clampForTerminationMessage(wire)), 0o600)

	if failed {
		return 1
	}
	return 0
}

// marshalRunResult serializes the run result, never returning empty. If it
// fails to marshal — e.g. a non-JSON Output slipped into the json.RawMessage —
// emit a minimal Failed result rather than silently writing nothing. A
// swallowed marshal error here is exactly how a non-JSON harness answer used to
// surface as silently-empty AgentRun output.
func marshalRunResult(wire agentruntime.RunResult) []byte {
	b, err := json.Marshal(wire)
	if err == nil {
		return b
	}
	b, _ = json.Marshal(map[string]string{
		"phase": "Failed",
		"error": "agent: run result could not be serialized: " + err.Error(),
	})
	return b
}

// terminationMessageBudget bounds the bytes written to /dev/termination-log.
// The kubelet caps the termination message near 4 KiB and truncates it
// silently; we stay under that with headroom so the JSON the controller
// unmarshals is always complete.
const terminationMessageBudget = 3072

// clampForTerminationMessage returns a copy of wire trimmed to fit the kubelet's
// termination-message cap. The full, untrimmed result is written to stdout (pod
// logs); this only bounds the controller's primary signal. It sheds detail in
// order of least value — a large output (as before), then tool-call argument /
// result bodies, then the step trace entirely — always preserving
// phase/usage/reason, which is what the controller folds for the run's verdict.
func clampForTerminationMessage(wire agentruntime.RunResult) agentruntime.RunResult {
	if len(wire.Output) > 2048 {
		wire.Output = json.RawMessage(`"<truncated; see pod logs>"`)
	}
	if termMessageFits(wire) {
		return wire
	}
	wire.Steps = elideStepPayloads(wire.Steps)
	if termMessageFits(wire) {
		return wire
	}
	// Drop the steps entirely, but keep the trace summary honest about it (M2.3):
	// the step/tool-call counts already survive in wire.Trace.
	if wire.Trace != nil {
		if b, err := json.Marshal(wire.Steps); err == nil {
			wire.Trace.DroppedBytes = int64(len(b))
		}
		wire.Trace.Truncated = true
	}
	wire.Steps = nil
	return wire
}

func termMessageFits(wire agentruntime.RunResult) bool {
	b, err := json.Marshal(wire)
	return err == nil && len(b) <= terminationMessageBudget
}

// overflowSinkFromEnv builds the trace-overflow S3 sink from the AGENTFS_S3_* env
// (the run's own object store). Returns nil when no bucket is set — a run with no
// object store keeps the full trace in pod logs only (M2.9, "no-op without
// creds"). Object credentials come from the pod's ambient AWS identity (IRSA),
// not a tenant secret; SSE inherits the bucket config so the overflow object is
// encrypted like every other AgentFS object.
func overflowSinkFromEnv(ctx context.Context) agentfs.S3 {
	bucket := os.Getenv("AGENTFS_S3_BUCKET")
	if bucket == "" {
		return nil
	}
	s3, err := agentfs.NewAWSS3(ctx, agentfs.AWSS3Config{
		Bucket:         bucket,
		Region:         os.Getenv("AGENTFS_S3_REGION"),
		Endpoint:       os.Getenv("AGENTFS_S3_ENDPOINT"),
		ForcePathStyle: os.Getenv("AGENTFS_S3_FORCE_PATH_STYLE") == "true",
		SSEAlgorithm:   os.Getenv("AGENTFS_S3_SSE"),
		KMSKeyARN:      os.Getenv("AGENTFS_S3_KMS_KEY_ARN"),
	})
	if err != nil {
		return nil
	}
	return s3
}

// overflowKey is the per-tenant (D1) key the full RunResult is parked at when the
// termination message can't hold the step detail.
func overflowKey(ns, run string) string {
	if ns == "" {
		ns = "default"
	}
	if run == "" {
		run = "run"
	}
	return fmt.Sprintf("runs/%s/%s/trace.json", ns, run)
}

// stampOverflowTrace uploads the full RunResult JSON to the sink and stamps
// Trace.OverflowRef so the clamped termination message points at it (M2.9).
// Best-effort: a Put error leaves the ref empty (the full result is still in the
// stdout logs). The SSE algorithm is inherited from the sink's bucket config.
func stampOverflowTrace(wire agentruntime.RunResult, sink agentfs.S3, ns, run string) agentruntime.RunResult {
	body, err := json.Marshal(wire)
	if err != nil {
		return wire
	}
	key := overflowKey(ns, run)
	if _, err := sink.Put(key, bytes.NewReader(body), agentfs.PutMeta{ContentType: "application/json"}); err != nil {
		return wire
	}
	if wire.Trace == nil {
		wire.Trace = &v1.TraceSummary{}
	}
	wire.Trace.OverflowRef = key
	return wire
}

// elideStepPayloads copies steps, stripping each tool call's argument and result
// bodies while keeping the lightweight skeleton (kind, tokens, timing, error,
// tool names). The full payloads remain in the stdout logs.
func elideStepPayloads(steps []v1.Step) []v1.Step {
	if len(steps) == 0 {
		return steps
	}
	out := make([]v1.Step, len(steps))
	for i, s := range steps {
		if len(s.ToolCalls) > 0 {
			tcs := make([]v1.ToolCallRecord, len(s.ToolCalls))
			for j, tc := range s.ToolCalls {
				// Record the elided sizes before dropping payloads, so the trace
				// stays honest about what was removed (M2.3).
				tc.ArgsBytes = int64(len(tc.Arguments))
				tc.ResultBytes = int64(len(tc.Result))
				tc.Arguments = nil
				tc.Result = nil
				tcs[j] = tc
			}
			s.ToolCalls = tcs
		}
		out[i] = s
	}
	return out
}

// waitForBrokerSocket reports whether the secret-broker UDS is usable. The
// broker runs as a sidecar that starts in parallel with this container, so the
// socket can lag our startup. The operator mounts the broker's EmptyDir (the
// socket's parent dir) only when it injects the sidecar, so a present directory
// means "a broker is attached" and we poll until the socket binds or timeout
// elapses; an absent directory means no broker, so we return immediately.
func waitForBrokerSocket(socket string, timeout time.Duration) bool {
	if fi, err := os.Stat(filepath.Dir(socket)); err != nil || !fi.IsDir() {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		if fi, err := os.Stat(socket); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// buildLoopLLM constructs the Mode=loop LLM client from the mounted
// provider.json (the operator renders it from the Agent's ModelProvider). The
// API key is leased from the broker by name — never embedded in the spec.
// Returns nil for harness agents (no provider.json); the executor uses the LLM
// only in loop mode.
func buildLoopLLM(ctx context.Context, dir string, leaser agentruntime.SecretLeaser) agentruntime.LLM {
	b, err := os.ReadFile(filepath.Join(dir, "provider.json")) // matches builders.runSpecProviderFile
	if err != nil {
		return nil
	}
	var p struct {
		Kind       string `json:"kind"`
		Endpoint   string `json:"endpoint"`
		ChatPath   string `json:"chatPath"`
		SecretName string `json:"secretName"`
	}
	if json.Unmarshal(b, &p) != nil || p.Endpoint == "" {
		return nil
	}
	var key string
	if p.SecretName != "" && leaser != nil {
		if v, lerr := leaser.LeaseSecret(ctx, p.SecretName, 0); lerr == nil {
			key = string(v)
		}
	}
	return openaillm.NewWithPath(p.Endpoint, key, p.ChatPath)
}
