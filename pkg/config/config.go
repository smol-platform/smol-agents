package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Mode mirrors identity.Mode but lives here to avoid import cycles.
type Mode string

const (
	ModeInsecure   Mode = "insecure"
	ModePermissive Mode = "permissive"
	ModeStrict     Mode = "strict"
)

func (m Mode) Valid() bool {
	switch m {
	case ModeInsecure, ModePermissive, ModeStrict:
		return true
	}
	return false
}

// Agent is the top-level configuration for cmd/agent.
//
// The shape is the same one shown in design.md. Defaults are applied in
// applyDefaults; validation in Validate.
type Agent struct {
	Mode          Mode          `yaml:"mode"`
	TrustDomain   string        `yaml:"trustDomain"`
	Identity      Identity      `yaml:"identity"`
	Transport     Transport     `yaml:"transport"`
	Secrets       Secrets       `yaml:"secrets"`
	EBPF          EBPF          `yaml:"ebpf"`
	Runtime       Runtime       `yaml:"runtime"`
	Sandbox       Sandbox       `yaml:"sandbox"`
	Observability Observability `yaml:"observability"`
}

type Identity struct {
	WorkloadAPI       string        `yaml:"workloadAPI"`
	BootTimeout       time.Duration `yaml:"bootTimeout"`
	MaxJWTLifetime    time.Duration `yaml:"maxJWTLifetime"`
	RotationThreshold float64       `yaml:"rotationThreshold"`
}

type Transport struct {
	Private PrivateTransport `yaml:"private"`
	Public  PublicTransport  `yaml:"public"`
}

type PrivateTransport struct {
	Addr      string   `yaml:"addr"`
	Authorize []string `yaml:"authorize"`
}

type PublicTransport struct {
	Addr     string   `yaml:"addr"`
	CertPath string   `yaml:"certPath"`
	KeyPath  string   `yaml:"keyPath"`
	Bind     []string `yaml:"bind"` // SPIFFE IDs allowed to terminate this listener
}

type Secrets struct {
	BrokerSocket string        `yaml:"brokerSocket"`
	MaxLeaseTTL  time.Duration `yaml:"maxLeaseTTL"`
}

type EBPF struct {
	Programs       []string `yaml:"programs"`
	ObjectsDir     string   `yaml:"objectsDir"`
	RingBufferSize int      `yaml:"ringBufferSize"`
}

type Runtime struct {
	DrainTimeout    time.Duration `yaml:"drainTimeout"`
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout"`
	HealthAddr      string        `yaml:"healthAddr"`
}

type Sandbox struct {
	RuntimeClass    string `yaml:"runtimeClass"`
	AllowHostEscape bool   `yaml:"allowHostEscape"`
}

type Observability struct {
	OTLPEndpoint string `yaml:"otlpEndpoint"`
	ServiceName  string `yaml:"serviceName"`
}

// LoadAgent reads, validates, and returns Agent config. Path may be empty,
// in which case all values come from defaults + environment overrides.
func LoadAgent(path string) (Agent, error) {
	cfg := Agent{}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("config: read %q: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %q: %w", path, err)
		}
	}
	cfg.applyDefaults()
	cfg.applyEnvOverrides()
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("config: validate: %w", err)
	}
	return cfg, nil
}

func (a *Agent) applyDefaults() {
	if a.Mode == "" {
		a.Mode = ModeStrict
	}
	if a.TrustDomain == "" {
		a.TrustDomain = "smol-agents.ai"
	}
	if a.Identity.WorkloadAPI == "" {
		a.Identity.WorkloadAPI = "unix:///run/spire/agent-sockets/api.sock"
	}
	if a.Identity.BootTimeout == 0 {
		a.Identity.BootTimeout = 30 * time.Second
	}
	if a.Identity.MaxJWTLifetime == 0 {
		a.Identity.MaxJWTLifetime = 5 * time.Minute
	}
	if a.Identity.RotationThreshold == 0 {
		a.Identity.RotationThreshold = 0.5 // R-IDN-1: rotate at 50% remaining
	}
	if a.Transport.Private.Addr == "" {
		a.Transport.Private.Addr = "0.0.0.0:8443"
	}
	if a.Secrets.BrokerSocket == "" {
		a.Secrets.BrokerSocket = "/run/secret-broker/secret-broker.sock"
	}
	if a.Secrets.MaxLeaseTTL == 0 {
		a.Secrets.MaxLeaseTTL = 15 * time.Minute
	}
	if a.EBPF.RingBufferSize == 0 {
		a.EBPF.RingBufferSize = 1 << 20 // 1 MiB
	}
	if a.EBPF.ObjectsDir == "" {
		a.EBPF.ObjectsDir = "/usr/share/smol-agents/bpf"
	}
	if a.Runtime.DrainTimeout == 0 {
		a.Runtime.DrainTimeout = 30 * time.Second
	}
	if a.Runtime.ShutdownTimeout == 0 {
		a.Runtime.ShutdownTimeout = 5 * time.Second
	}
	if a.Runtime.HealthAddr == "" {
		a.Runtime.HealthAddr = "0.0.0.0:8080"
	}
	if a.Sandbox.RuntimeClass == "" {
		a.Sandbox.RuntimeClass = "kata-fc"
	}
	if a.Observability.ServiceName == "" {
		a.Observability.ServiceName = "smol-agent"
	}
}

func (a *Agent) applyEnvOverrides() {
	if v := os.Getenv("SMOL_AGENTS_MODE"); v != "" {
		a.Mode = Mode(strings.ToLower(v))
	}
	if v := os.Getenv("SMOL_AGENTS_TRUST_DOMAIN"); v != "" {
		a.TrustDomain = v
	}
	if v := os.Getenv("SMOL_AGENTS_WORKLOAD_API"); v != "" {
		a.Identity.WorkloadAPI = v
	}
	if v := os.Getenv("SMOL_AGENTS_BROKER_SOCKET"); v != "" {
		a.Secrets.BrokerSocket = v
	}
	if v := os.Getenv("SMOL_AGENTS_OTLP_ENDPOINT"); v != "" {
		a.Observability.OTLPEndpoint = v
	}
}

// Validate checks invariants required by the requirements doc.
func (a Agent) Validate() error {
	var errs []error
	if !a.Mode.Valid() {
		errs = append(errs, fmt.Errorf("mode %q is not insecure|permissive|strict", a.Mode))
	}
	if a.Mode == ModeInsecure && os.Getenv("SMOL_AGENTS_ALLOW_INSECURE") != "1" {
		// R-IDN-3 acceptance #3
		errs = append(errs, errors.New("mode=insecure requires SMOL_AGENTS_ALLOW_INSECURE=1"))
	}
	if a.TrustDomain == "" {
		errs = append(errs, errors.New("trustDomain is required"))
	}
	if a.Identity.RotationThreshold <= 0 || a.Identity.RotationThreshold >= 1 {
		errs = append(errs, errors.New("identity.rotationThreshold must be in (0,1)"))
	}
	if a.Transport.Private.Addr != "" {
		if _, _, err := net.SplitHostPort(a.Transport.Private.Addr); err != nil {
			errs = append(errs, fmt.Errorf("transport.private.addr invalid: %w", err))
		}
	}
	if a.Transport.Public.Addr != "" {
		if _, _, err := net.SplitHostPort(a.Transport.Public.Addr); err != nil {
			errs = append(errs, fmt.Errorf("transport.public.addr invalid: %w", err))
		}
		if a.Transport.Public.CertPath == "" || a.Transport.Public.KeyPath == "" {
			// R-MTL-2 acceptance #1
			errs = append(errs, errors.New("transport.public requires certPath and keyPath"))
		}
	}
	if a.Secrets.MaxLeaseTTL <= 0 {
		errs = append(errs, errors.New("secrets.maxLeaseTTL must be > 0"))
	}
	if a.Runtime.DrainTimeout <= 0 {
		errs = append(errs, errors.New("runtime.drainTimeout must be > 0"))
	}
	return errors.Join(errs...)
}
