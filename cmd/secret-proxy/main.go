// Command secret-proxy is the kloak-style secret broker sidecar.
//
// It listens on a UDS, authenticates each peer via SO_PEERCRED + SPIRE,
// enforces a static policy, and brokers secrets from a backend.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"gopkg.in/yaml.v3"

	"github.com/smol-platform/smol-agents/internal/version"
	"github.com/smol-platform/smol-agents/pkg/observability"
	"github.com/smol-platform/smol-agents/pkg/secrets"
)

// brokerConfig describes the broker's YAML configuration.
type brokerConfig struct {
	SocketPath      string `yaml:"socketPath"`
	WorkloadAPIAddr string `yaml:"workloadAPI"`
	// PeerAuth selects how peers are attested: "spire" (default) uses the
	// SPIRE workload API; "local" uses SO_PEERCRED uid (no SPIRE); "spire+local"
	// tries SPIRE then falls back to local.
	PeerAuth         string        `yaml:"peerAuth"`
	LocalTrustDomain string        `yaml:"localTrustDomain"` // synthetic TD for peerAuth=local
	MaxLeaseTTL      time.Duration `yaml:"maxLeaseTTL"`
	DefaultTTL       time.Duration `yaml:"defaultTTL"`
	Backend          struct {
		Kind   string `yaml:"kind"` // "static" | "vault"
		Static []struct {
			SPIFFEID string            `yaml:"spiffeID"`
			Items    map[string]string `yaml:"items"`
		} `yaml:"static"`
	} `yaml:"backend"`
	Policy []struct {
		SPIFFEID string   `yaml:"spiffeID"`
		Allow    []string `yaml:"allow"`
	} `yaml:"policy"`
}

func main() {
	configPath := flag.String("config", "/etc/secret-proxy/config.yaml", "broker config")
	logLevel := flag.String("log-level", "info", "debug|info|warn|error")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		os.Stdout.WriteString(version.String() + "\n")
		return
	}

	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(*logLevel))
	logger := observability.MustLogger(level)

	cfg, err := loadBrokerConfig(*configPath)
	if err != nil {
		logger.Error("config", "err", err)
		os.Exit(2)
	}

	backend, err := buildBackend(cfg)
	if err != nil {
		logger.Error("backend", "err", err)
		os.Exit(2)
	}
	policy, err := buildPolicy(cfg)
	if err != nil {
		logger.Error("policy", "err", err)
		os.Exit(2)
	}
	attestor, err := buildAttestor(cfg)
	if err != nil {
		logger.Error("attestor", "err", err)
		os.Exit(2)
	}

	srv := &secrets.Server{
		SocketPath:  cfg.SocketPath,
		MaxLeaseTTL: cfg.MaxLeaseTTL,
		DefaultTTL:  cfg.DefaultTTL,
		Backend:     backend,
		Policy:      policy,
		Attestor:    attestor,
		Logger:      logger,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("secret-proxy starting",
		"version", version.Version,
		"socket", cfg.SocketPath,
		"backend", cfg.Backend.Kind,
	)
	if err := srv.Listen(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("listen", "err", err)
		os.Exit(1)
	}
}

func loadBrokerConfig(path string) (brokerConfig, error) {
	var cfg brokerConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = "/run/secret-broker/secret-broker.sock"
	}
	if cfg.WorkloadAPIAddr == "" {
		cfg.WorkloadAPIAddr = "unix:///run/spire/agent-sockets/api.sock"
	}
	if cfg.MaxLeaseTTL <= 0 {
		cfg.MaxLeaseTTL = 15 * time.Minute
	}
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = 10 * time.Minute
	}
	if cfg.Backend.Kind == "" {
		cfg.Backend.Kind = "static"
	}
	return cfg, nil
}

// buildAttestor selects the peer attestor per cfg.PeerAuth. "spire" (default)
// needs the SPIRE workload API; "local" uses SO_PEERCRED uid (no SPIRE, for
// in-pod brokers); "spire+local" prefers SPIRE and falls back to local.
func buildAttestor(cfg brokerConfig) (secrets.PeerAttestor, error) {
	switch cfg.PeerAuth {
	case "", "spire":
		return secrets.NewSPIREPeerAttestor(cfg.WorkloadAPIAddr)
	case "local":
		return secrets.NewLocalPeerAttestor(cfg.LocalTrustDomain)
	case "spire+local":
		s, err := secrets.NewSPIREPeerAttestor(cfg.WorkloadAPIAddr)
		if err != nil {
			return nil, err
		}
		l, err := secrets.NewLocalPeerAttestor(cfg.LocalTrustDomain)
		if err != nil {
			return nil, err
		}
		return secrets.MultiAttestor{s, l}, nil
	default:
		return nil, errors.New("unknown peerAuth: " + cfg.PeerAuth)
	}
}

func buildBackend(cfg brokerConfig) (secrets.Backend, error) {
	switch cfg.Backend.Kind {
	case "static":
		b := secrets.NewStaticBackend()
		for _, entry := range cfg.Backend.Static {
			id, err := spiffeid.FromString(entry.SPIFFEID)
			if err != nil {
				return nil, err
			}
			for name, value := range entry.Items {
				b.Set(id, name, []byte(value))
			}
		}
		return b, nil
	case "vault":
		return nil, errors.New("vault backend not yet wired in this build")
	default:
		return nil, errors.New("unknown backend kind: " + cfg.Backend.Kind)
	}
}

func buildPolicy(cfg brokerConfig) (secrets.Policy, error) {
	p := secrets.NewStaticPolicy()
	for _, e := range cfg.Policy {
		id, err := spiffeid.FromString(e.SPIFFEID)
		if err != nil {
			return nil, err
		}
		p.Grant(id, e.Allow...)
	}
	return p, nil
}
