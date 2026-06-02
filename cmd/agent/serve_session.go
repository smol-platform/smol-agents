package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentruntime"
	"github.com/smol-platform/smol-agents/pkg/observability"
	"github.com/smol-platform/smol-agents/pkg/secrets"
)

// runServeSession is the `agent serve-session` subcommand: the long-running
// runtime behind an AgentSession. It loads the mounted Agent spec, restores
// durable session state from the AgentFS workspace (the init container already
// restored the files), and drives agentruntime.SessionWorker — processing turns
// from the inbox, checkpointing after each, parking when idle — until SIGTERM
// (final checkpoint) or the idle timeout (exit so Knative can scale to zero).
func runServeSession(args []string) int {
	fs := flag.NewFlagSet("serve-session", flag.ExitOnError)
	dir := fs.String("dir", "/etc/smol-agents/run", "directory with agent.json")
	workspace := fs.String("workspace", "", "session workspace (AgentFS mount); default = agent working dir")
	agentRef := fs.String("agent-ref", "", "AgentSession's agentRef (recorded in checkpoint metadata)")
	socket := fs.String("secret-socket", "/run/secret-broker/secret-broker.sock", "secret broker UDS")
	poll := fs.Duration("poll", 2*time.Second, "inbox poll interval")
	idle := fs.Duration("idle-timeout", 0, "exit (scale-to-zero) after this idle; 0 = never")
	_ = fs.Parse(args)

	logger := observability.MustLogger(slog.LevelInfo)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	agent, err := loadAgentSpec(filepath.Join(*dir, agentruntime.AgentSpecFile))
	if err != nil {
		logger.Error("serve-session: load agent spec", "err", err)
		return 2
	}

	ws := *workspace
	if ws == "" {
		ws = agent.Spec.EffectiveWorkingDir()
	}
	if ws == "" {
		logger.Error("serve-session: no workspace; set --workspace or the Agent's storage.agentfs")
		return 2
	}

	// Broker leaser (when the sidecar is attached) + loop LLM, same as `agent run`.
	var leaser agentruntime.SecretLeaser
	if waitForBrokerSocket(*socket, 60*time.Second) {
		leaser = secretLeaser{c: secrets.NewClient(*socket)}
	}

	w := &agentruntime.SessionWorker{
		Agent:        agent,
		AgentRef:     *agentRef,
		Workspace:    ws,
		Leaser:       leaser,
		LLM:          buildLoopLLM(ctx, *dir, leaser),
		PollInterval: *poll,
		IdleTimeout:  *idle,
		Logger:       logger,
	}
	logger.Info("serving AgentSession", "workspace", ws, "poll", poll.String(), "idleTimeout", idle.String())
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("session worker", "err", err)
		return 1
	}
	logger.Info("session worker stopped", "reason", ctx.Err())
	return 0
}

func loadAgentSpec(path string) (v1.Agent, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return v1.Agent{}, err
	}
	var a v1.Agent
	return a, json.Unmarshal(b, &a)
}
