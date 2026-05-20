package v1

// FeatureBase is embedded by every feature spec. It carries the
// universal `enabled` boolean and a per-feature rollout policy.
type FeatureBase struct {
	// Enabled toggles the entire feature. When false the operator
	// removes any owned resources for the feature.
	// +kubebuilder:default:=true
	Enabled bool `json:"enabled"`

	// RolloutPolicy is one of Immediate | Canary | Manual.
	// +kubebuilder:validation:Enum=Immediate;Canary;Manual
	// +kubebuilder:default:=Immediate
	RolloutPolicy string `json:"rolloutPolicy,omitempty"`
}

// IdentityFeature configures the SPIFFE workload identity feature.
// Implements R-IDN-* through the agent ConfigMap and ClusterSPIFFEID.
type IdentityFeature struct {
	FeatureBase `json:",inline"`

	// Mode is one of insecure | permissive | strict.
	// +kubebuilder:validation:Enum=insecure;permissive;strict
	// +kubebuilder:default:=strict
	Mode string `json:"mode,omitempty"`

	// WorkloadAPI is the SPIRE workload API socket path.
	// +kubebuilder:default:="unix:///run/spire/agent-sockets/api.sock"
	WorkloadAPI string `json:"workloadAPI,omitempty"`

	// BootTimeoutSeconds is how long the agent waits for an SVID.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=30
	BootTimeoutSeconds int32 `json:"bootTimeoutSeconds,omitempty"`
}

// TransportFeature carries both private and public sub-features.
type TransportFeature struct {
	Private TransportPrivateFeature `json:"private,omitempty"`
	Public  TransportPublicFeature  `json:"public,omitempty"`
}

// TransportPrivateFeature configures in-mesh SPIFFE mTLS.
type TransportPrivateFeature struct {
	FeatureBase `json:",inline"`

	// +kubebuilder:default:="0.0.0.0:8443"
	Addr string `json:"addr,omitempty"`

	// Authorize lists SPIFFE authorizer descriptors. Each is one of
	// `any:spiffe://td`, `prefix:spiffe://td/path`, or a fully-qualified
	// SPIFFE ID. OR semantics across entries.
	Authorize []string `json:"authorize,omitempty"`
}

// TransportPublicFeature configures gateway-fronted public mTLS.
// Default-disabled; tenants must explicitly opt-in.
type TransportPublicFeature struct {
	FeatureBase `json:",inline"`

	// +kubebuilder:default:="0.0.0.0:8444"
	Addr string `json:"addr,omitempty"`

	CertPath string `json:"certPath,omitempty"`
	KeyPath  string `json:"keyPath,omitempty"`
}

// SecretsFeature configures the kloak-style broker sidecar.
type SecretsFeature struct {
	FeatureBase `json:",inline"`

	// +kubebuilder:default:="/run/secret-broker/secret-broker.sock"
	BrokerSocket string `json:"brokerSocket,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=900
	MaxLeaseTTLSeconds int32 `json:"maxLeaseTTLSeconds,omitempty"`
}

// SandboxFeature controls the gVisor RuntimeClass propagation.
type SandboxFeature struct {
	FeatureBase `json:",inline"`

	// +kubebuilder:default:="kata-fc"
	RuntimeClass string `json:"runtimeClass,omitempty"`

	// AllowHostEscape lets a tenant opt-out of sandboxing. Mirrors the
	// chart's R-SBX-1 guard; the validating webhook rejects this unless
	// the platform CR allows it.
	AllowHostEscape bool `json:"allowHostEscape,omitempty"`
}

// EBPFFeature controls the per-Pod eBPF program list (the host-level
// loader is governed by the Platform CR, not this feature).
type EBPFFeature struct {
	FeatureBase `json:",inline"`

	// +kubebuilder:default:={"syscalls","network"}
	Programs []string `json:"programs,omitempty"`
}

// KnativeFeature controls Knative Serving exposure.
type KnativeFeature struct {
	FeatureBase `json:",inline"`

	// +kubebuilder:default:=true
	ScaleToZero bool `json:"scaleToZero,omitempty"`

	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default:=0
	MinScale int32 `json:"minScale,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default:=50
	MaxScale int32 `json:"maxScale,omitempty"`
}

// ObservabilityFeature configures OTel emission.
type ObservabilityFeature struct {
	FeatureBase `json:",inline"`

	OTLPEndpoint string `json:"otlpEndpoint,omitempty"`

	// +kubebuilder:default:="smol-agent"
	ServiceName string `json:"serviceName,omitempty"`
}

// Features groups all per-feature configuration.
type Features struct {
	Identity      IdentityFeature      `json:"identity,omitempty"`
	Transport     TransportFeature     `json:"transport,omitempty"`
	Secrets       SecretsFeature       `json:"secrets,omitempty"`
	Sandbox       SandboxFeature       `json:"sandbox,omitempty"`
	EBPF          EBPFFeature          `json:"ebpf,omitempty"`
	Knative       KnativeFeature       `json:"knative,omitempty"`
	Observability ObservabilityFeature `json:"observability,omitempty"`
}

// FeaturePolicyRow declares whether a feature is allowed cluster-wide
// and its default `enabled` value when a tenant CR omits it.
type FeaturePolicyRow struct {
	// +kubebuilder:validation:MinLength=1
	Feature string `json:"feature"`

	// +kubebuilder:default:=true
	Allowed bool `json:"allowed"`

	// +kubebuilder:default:=true
	DefaultEnabled bool `json:"defaultEnabled"`
}
