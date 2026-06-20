package v1

import (
	"errors"
	"fmt"
	"strings"
)

// ModelGatewaySpec declares an operator-managed model/agent gateway (yxh.2): a
// thin lifecycle+isolation wrapper that has the operator render+harden the
// gateway's Deployment+Service+config, instead of deploying it as a manual,
// unmanaged workload. Provider="hermes" is the first implementation (the
// NousResearch nousresearch/hermes-agent OpenAI-compatible gateway).
//
// The CRD is deliberately THIN: the request surface (chat/responses/runs, model,
// server-side levers) stays in the harness + the gateway's own config file —
// only the deployment lifecycle and isolation are modeled here. A gateway is
// host-level RCE, so the operator runs it under the same sandbox + egress floor
// as run pods (kata-fc by default).
type ModelGatewaySpec struct {
	// Provider selects the gateway implementation. Only "hermes" exists today;
	// it sets the container args ("gateway run"), the API_SERVER_*/HERMES_HOME
	// env conventions, and the config-seed init container.
	// +kubebuilder:validation:Enum=hermes
	Provider string `json:"provider"`

	// Image is the gateway container image (e.g. nousresearch/hermes-agent:latest).
	Image string `json:"image"`

	// Port is the gateway's listen port. 0 defaults per provider (hermes: 8642).
	// +kubebuilder:validation:Minimum=0
	// +optional
	Port int32 `json:"port,omitempty"`

	// Config is the gateway's config-file content, rendered into a ConfigMap and
	// seeded into the gateway's data dir by an init container. For hermes this is
	// config.yaml (the model source + server-side levers). +optional
	Config string `json:"config,omitempty"`

	// Env are extra environment variables for the gateway container — provider
	// keys (via secretRef, broker/agent-blind), base URLs, model selectors. The
	// operator adds the provider's own conventions on top. +optional
	Env []HarnessEnvVar `json:"env,omitempty"`

	// Sandbox overrides the pod RuntimeClass. Empty inherits the operator's
	// --default-run-runtime-class (kata-fc). The gateway is RCE, so a microVM is
	// recommended; runc requires the operator's --allow-host-runtime. +optional
	Sandbox SandboxSpec `json:"sandbox,omitempty"`

	// Replicas is the gateway Deployment replica count (default 1). +optional
	// +kubebuilder:validation:Minimum=0
	Replicas *int32 `json:"replicas,omitempty"`
}

// ModelGatewayStatus is the observed gateway state.
type ModelGatewayStatus struct {
	// Phase: Pending | Ready | Failed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Endpoint is the in-cluster base URL agents point harness.http.url at
	// (append the API path, e.g. /v1/chat/completions). Empty until the gateway
	// Service is rendered. +optional
	Endpoint string `json:"endpoint,omitempty"`
	// +optional
	Reason string `json:"reason,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// HermesDefaultPort is the nousresearch/hermes-agent API server port.
const HermesDefaultPort int32 = 8642

// EffectivePort resolves the gateway listen port, defaulting per provider.
func (s ModelGatewaySpec) EffectivePort() int32 {
	if s.Port > 0 {
		return s.Port
	}
	if s.Provider == "hermes" {
		return HermesDefaultPort
	}
	return 0
}

// ValidateModelGateway enforces the spec invariants: a known provider, an image,
// a resolvable port, and well-formed env (a secretRef OR a literal, not both).
func ValidateModelGateway(s ModelGatewaySpec) error {
	var errs []error
	switch s.Provider {
	case "hermes":
	case "":
		errs = append(errs, errors.New("modelGateway.provider is required"))
	default:
		errs = append(errs, fmt.Errorf("modelGateway.provider=%q is invalid (only hermes today)", s.Provider))
	}
	if strings.TrimSpace(s.Image) == "" {
		errs = append(errs, errors.New("modelGateway.image is required"))
	}
	if s.EffectivePort() <= 0 {
		errs = append(errs, errors.New("modelGateway.port is required (no provider default)"))
	}
	for i, e := range s.Env {
		if strings.TrimSpace(e.Name) == "" {
			errs = append(errs, fmt.Errorf("modelGateway.env[%d].name is required", i))
		}
		if e.SecretRef != nil && strings.TrimSpace(e.SecretRef.SecretName) == "" {
			errs = append(errs, fmt.Errorf("modelGateway.env[%d].secretRef.secretName is required", i))
		}
	}
	return errors.Join(errs...)
}
