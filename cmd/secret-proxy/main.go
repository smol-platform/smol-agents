// Command secret-proxy is the kloak-style secret broker sidecar.
//
// It listens on a UDS, authenticates each peer via SO_PEERCRED + SPIRE,
// enforces a static policy, and brokers secrets from a backend.
package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"gopkg.in/yaml.v3"

	"github.com/smol-platform/smol-agents/internal/version"
	"github.com/smol-platform/smol-agents/pkg/observability"
	"github.com/smol-platform/smol-agents/pkg/secrets"
	"github.com/smol-platform/smol-agents/pkg/trat"
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
		// Dynamic is an INDEPENDENT block (not selected by Kind): when present it
		// enables the dynamic provider-credential mint path (D8) alongside the
		// static backend. Requires peerAuth=spire + a tts{} verifier.
		Dynamic *struct {
			Provider         string                       `yaml:"provider"` // "githubApp"
			AppID            string                       `yaml:"appID"`
			PrivateKeyPath   string                       `yaml:"privateKeyPath"` // mounted root secret (PEM)
			BaseURL          string                       `yaml:"baseURL"`
			ScopePermissions map[string]map[string]string `yaml:"scopePermissions"`
		} `yaml:"dynamic"`
	} `yaml:"backend"`
	Policy []struct {
		SPIFFEID string   `yaml:"spiffeID"`
		Allow    []string `yaml:"allow"`
	} `yaml:"policy"`
	// TTS configures the TraT verifier (JWKS source + expected audience) for the
	// dynamic mint path. Required when backend.dynamic is set.
	TTS *struct {
		JWKSURL  string `yaml:"jwksURL"`
		Audience string `yaml:"audience"`
	} `yaml:"tts"`
	// CredentialPolicy is the deny-by-default mint allow-list (per-principal
	// scope→credential→repos), built into a StaticCredentialPolicy.
	CredentialPolicy []struct {
		SPIFFEID   string   `yaml:"spiffeID"`
		Scope      string   `yaml:"scope"`
		Credential string   `yaml:"credential"`
		Repos      []string `yaml:"repos"`
	} `yaml:"credentialPolicy"`
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
	// Dynamic provider-credential mint path (D8) — nil unless backend.dynamic is
	// set; fails closed (peerAuth=spire required) at startup.
	dynamic, verifier, credPolicy, err := buildDynamic(cfg)
	if err != nil {
		logger.Error("dynamic", "err", err)
		os.Exit(2)
	}

	srv := &secrets.Server{
		SocketPath:   cfg.SocketPath,
		MaxLeaseTTL:  cfg.MaxLeaseTTL,
		DefaultTTL:   cfg.DefaultTTL,
		Backend:      backend,
		Policy:       policy,
		Attestor:     attestor,
		Logger:       logger,
		Dynamic:      dynamic,
		TraTVerifier: verifier,
		CredPolicy:   credPolicy,
		// Interactive-caller policy (M4.12): classify each UDS peer (agent vs a
		// PTY-spawned driver shell) and apply the knob. Empty env = allow-audited
		// (the default): a driver shell still leases but the access is audited;
		// set to "deny" to refuse leases to interactive callers.
		InteractivePolicy: secrets.InteractiveCallerPolicy(os.Getenv("SMOL_AGENTS_INTERACTIVE_CALLER_POLICY")),
		ClassifyConn:      func(c net.Conn) secrets.CallerClass { return secrets.PeerCallerClass(c, secrets.ProcfsAncestry{}) },
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

// buildDynamic wires the dynamic provider-credential mint path (D8) from the
// optional backend.dynamic block. Returns (nil,nil,nil,nil) when no dynamic
// block is set (the proxy is static-only, as today).
//
// SECURITY (M1.22): dynamic minting binds a sender-constrained TraT to the
// caller's SVID, so it REQUIRES SPIRE peer attestation. Refuse to start under
// peerAuth=local or spire+local — a local-uid fallback would let a peer present
// a TraT minted for a different SVID and defeat the sender-constraint. Only pure
// SPIRE ("" default, or "spire") is permitted.
func buildDynamic(cfg brokerConfig) (secrets.DynamicBackend, trat.Verifier, secrets.CredentialPolicy, error) {
	d := cfg.Backend.Dynamic
	if d == nil {
		return nil, nil, nil, nil
	}
	if cfg.PeerAuth != "" && cfg.PeerAuth != "spire" {
		return nil, nil, nil, fmt.Errorf("backend.dynamic requires peerAuth=spire (the TraT sender-constraint binds to the SVID); got %q", cfg.PeerAuth)
	}
	if d.Provider != "githubApp" {
		return nil, nil, nil, fmt.Errorf("backend.dynamic.provider=%q is invalid (only githubApp)", d.Provider)
	}
	if cfg.TTS == nil || cfg.TTS.JWKSURL == "" {
		return nil, nil, nil, errors.New("backend.dynamic requires a tts{} block with jwksURL (the TraT verifier)")
	}
	key, err := loadRSAKey(d.PrivateKeyPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load github app private key: %w", err)
	}
	dyn := &secrets.GitHubAppBackend{
		AppID:            d.AppID,
		PrivateKey:       key,
		BaseURL:          d.BaseURL,
		ScopePermissions: d.ScopePermissions,
	}
	verifier := &trat.JWKSVerifier{Keys: &trat.HTTPKeySource{URL: cfg.TTS.JWKSURL}, Audience: cfg.TTS.Audience}
	credPol, err := buildCredentialPolicy(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	return dyn, verifier, credPol, nil
}

// buildCredentialPolicy builds the deny-by-default mint allow-list from
// credentialPolicy[] (per-principal scope→credential→repos).
func buildCredentialPolicy(cfg brokerConfig) (secrets.CredentialPolicy, error) {
	p := secrets.NewStaticCredentialPolicy()
	for _, g := range cfg.CredentialPolicy {
		id, err := spiffeid.FromString(g.SPIFFEID)
		if err != nil {
			return nil, err
		}
		p.Grant(id, g.Scope, g.Credential, g.Repos...)
	}
	return p, nil
}

// loadRSAKey reads a PEM file and parses an RSA private key (PKCS#1 or PKCS#8).
func loadRSAKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("no PEM block in key file")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("key is not an RSA private key")
	}
	return rk, nil
}
