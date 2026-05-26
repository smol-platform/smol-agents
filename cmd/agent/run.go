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

	// Wire the broker leaser only when its socket is actually present — the
	// executor errors clearly if a secretRef is declared without it.
	var leaser agentruntime.SecretLeaser
	if fi, err := os.Stat(*socket); err == nil && fi.Mode()&os.ModeSocket != 0 {
		leaser = secretLeaser{c: secrets.NewClient(*socket)}
	}

	res, runErr := agentruntime.RunOnce(ctx, *dir, leaser, buildLoopLLM(ctx, *dir, leaser))
	wire := agentruntime.ResultToWire(res, runErr)

	// Full result to stdout for log-based debugging.
	full, _ := json.Marshal(wire)
	os.Stdout.Write(full)
	os.Stdout.Write([]byte("\n"))

	// Termination message must stay valid JSON within the kubelet's ~4KiB cap,
	// so truncate a large output there (the controller reads phase/usage/error
	// from it; full output is in the logs).
	if len(wire.Output) > 2048 {
		wire.Output = json.RawMessage(`"<truncated; see pod logs>"`)
	}
	if tm, err := json.Marshal(wire); err == nil {
		_ = os.WriteFile(*termLog, tm, 0o600)
	}

	if runErr != nil {
		return 1
	}
	return 0
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
