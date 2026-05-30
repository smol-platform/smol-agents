package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
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

	res, runErr := agentruntime.RunOnce(ctx, *dir, leaser, buildLoopLLM(ctx, *dir, leaser))
	wire := agentruntime.ResultToWire(res, runErr)

	// Full result to stdout for log-based debugging.
	os.Stdout.Write(marshalRunResult(wire))
	os.Stdout.Write([]byte("\n"))

	// The termination message is the controller's primary signal and must stay
	// valid JSON under the kubelet's ~4KiB cap. clampForTerminationMessage trims
	// it to fit so a large run can't overflow the cap and corrupt the JSON —
	// which would fail the controller's fold and silently zero the run. The
	// full, untrimmed result is in the stdout logs written above.
	_ = os.WriteFile(*termLog, marshalRunResult(clampForTerminationMessage(wire)), 0o600)

	if runErr != nil {
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
	wire.Steps = nil
	return wire
}

func termMessageFits(wire agentruntime.RunResult) bool {
	b, err := json.Marshal(wire)
	return err == nil && len(b) <= terminationMessageBudget
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
	return openaillm.New(p.Endpoint, key)
}
