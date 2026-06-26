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

	// UI opts the gateway's web UI (e.g. the Hermes dashboard) into a human-facing,
	// authenticated exposure. Off by default — the gateway port is machine-only
	// (agents dial it cross-pod with a bearer token). +optional
	UI *GatewayUISpec `json:"ui,omitempty"`
}

// GatewayUISpec exposes the gateway's web UI to humans behind a platform-managed
// auth front. The operator renders an auth-proxy sidecar in front of the gateway
// port plus a dedicated "<name>-ui" Service; a human reaches it via port-forward
// (today) or a real ingress (later). Generic across providers (Hermes dashboard,
// pi HTTP, …) — the gateway is unchanged; only the front is added.
type GatewayUISpec struct {
	// Expose turns on the authenticated UI surface. +optional
	Expose bool `json:"expose,omitempty"`

	// Port is the auth-proxy listen port (the UI Service port). 0 defaults to 8643.
	// Must differ from the gateway port. +kubebuilder:validation:Minimum=0 +optional
	Port int32 `json:"port,omitempty"`

	// Auth selects how humans authenticate to the UI.
	Auth GatewayUIAuth `json:"auth"`
}

// GatewayUIAuth is the human-auth front for the gateway UI.
type GatewayUIAuth struct {
	// Mode selects the auth front. "sharedSecret" = HTTP basic-auth from an
	// htpasswd Secret (impl #1). "oidc" = platform-managed OIDC (bundled
	// Dex/Keycloak per decision D9) — reserved, not yet implemented.
	// +kubebuilder:validation:Enum=sharedSecret;oidc
	Mode string `json:"mode"`

	// SecretRef points at the auth material. For sharedSecret: a Secret whose key
	// (default "htpasswd") holds one or more htpasswd lines (user:bcrypt). +optional
	SecretRef *AuthRef `json:"secretRef,omitempty"`
}

// GatewayUIDefaultPort is the default auth-proxy listen port.
const GatewayUIDefaultPort int32 = 8643

// EffectiveUIPort resolves the UI auth-proxy listen port, defaulting to 8643.
func (s ModelGatewaySpec) EffectiveUIPort() int32 {
	if s.UI != nil && s.UI.Port > 0 {
		return s.UI.Port
	}
	return GatewayUIDefaultPort
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
	// UIEndpoint is the in-cluster base URL of the authenticated UI Service (set
	// only when spec.ui.expose=true). Port-forward this to reach the dashboard.
	// +optional
	UIEndpoint string `json:"uiEndpoint,omitempty"`
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
	if s.UI != nil && s.UI.Expose {
		switch s.UI.Auth.Mode {
		case "sharedSecret":
			if s.UI.Auth.SecretRef == nil || strings.TrimSpace(s.UI.Auth.SecretRef.SecretName) == "" {
				errs = append(errs, errors.New("modelGateway.ui.auth.secretRef.secretName is required for mode=sharedSecret"))
			}
		case "oidc":
			errs = append(errs, errors.New("modelGateway.ui.auth.mode=oidc is not yet implemented (use sharedSecret)"))
		case "":
			errs = append(errs, errors.New("modelGateway.ui.auth.mode is required when ui.expose=true"))
		default:
			errs = append(errs, fmt.Errorf("modelGateway.ui.auth.mode=%q is invalid (sharedSecret|oidc)", s.UI.Auth.Mode))
		}
		if s.EffectiveUIPort() == s.EffectivePort() {
			errs = append(errs, errors.New("modelGateway.ui.port must differ from the gateway port"))
		}
	}
	return errors.Join(errs...)
}
