package v1

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// NetworkKind discriminates the AgentNetwork transport.
type NetworkKind string

const (
	NetworkIdentityProxy NetworkKind = "identityProxy"
	NetworkWireGuardMesh NetworkKind = "wireguardMesh"
)

func (k NetworkKind) Valid() bool {
	switch k {
	case NetworkIdentityProxy, NetworkWireGuardMesh:
		return true
	}
	return false
}

// AgentNetworkSpec is the canonical CRD shape. Implements R-AN-API-1.
type AgentNetworkSpec struct {
	// Kind discriminates the transport (validated by NetworkKind.Valid).
	// +kubebuilder:validation:Enum=identityProxy;wireguardMesh
	Kind NetworkKind `json:"kind"`

	// AgentSelector picks which Agents in the namespace get this
	// network's sidecar injected. If empty, the AgentNetwork is
	// available but binds nothing.
	AgentSelector map[string]string `json:"agentSelector,omitempty"`

	// IdentityProxy is required when kind=identityProxy.
	IdentityProxy *IdentityProxySpec `json:"identityProxy,omitempty"`

	// WireGuardMesh is required when kind=wireguardMesh.
	WireGuardMesh *WireGuardSpec `json:"wireguardMesh,omitempty"`
}

// IdentityProxySpec configures the SPIFFE-aware sidecar.
type IdentityProxySpec struct {
	Resources []ResourceTarget `json:"resources"`

	// +optional
	Egress EgressPolicy `json:"egress,omitempty"`

	// TTS configures the Tokenetes Transaction Token Service used to mint
	// (and verify) TraTs. Required when any resource sets trat or credential.
	// +optional
	TTS *TTSRef `json:"tts,omitempty"`
}

// TTSRef configures the Tokenetes TTS. R-TRAT-API-2 / R-SEGR.
type TTSRef struct {
	// URL is the TTS token-exchange endpoint (RFC 8693).
	URL string `json:"url"`
	// SubjectTokenType for the JWT-SVID subject_token; default
	// urn:ietf:params:oauth:token-type:jwt.
	// +optional
	SubjectTokenType string `json:"subjectTokenType,omitempty"`
	// SubjectAudience is the audience the JWT-SVID subject_token is minted
	// for (the TTS's identity); default = the TraT audience.
	// +optional
	SubjectAudience string `json:"subjectAudience,omitempty"`
	// JWKSURL is the TTS signing key set; required when a resource sets
	// credential (the broker verifies the TraT before minting). R-SEGR-AUTH-1.
	// +optional
	JWKSURL string `json:"jwksUrl,omitempty"`
}

// TraTInjection enables injecting a TraT (Txn-Token) on egress to a resource.
// R-TRAT-API-1.
type TraTInjection struct {
	// Scope is the RFC 8693 scope (the transaction intent) for this resource.
	Scope string `json:"scope"`
	// Audience overrides the trust-domain audience; default = platform trust domain.
	// +optional
	Audience string `json:"audience,omitempty"`
	// Header overrides the conveyance header; default "Txn-Token".
	// +optional
	Header string `json:"header,omitempty"`
}

// CredentialInjection enables secretless injection of a broker-minted provider
// credential on egress to a resource. The agent never sees the value.
// R-SEGR-API-1.
type CredentialInjection struct {
	// Name is the broker credential/policy key (e.g. "github").
	Name string `json:"name"`
	// Scope is the RFC 8693 transaction intent of the authorizing TraT the
	// broker verifies before minting. This TraT is internal-only (never sent
	// to the upstream). R-SEGR-AUTH-1.
	Scope string `json:"scope"`
	// Header overrides where the value is placed; default "Authorization".
	// +optional
	Header string `json:"header,omitempty"`
	// Scheme prefixes the value; default "Bearer".
	// +optional
	Scheme string `json:"scheme,omitempty"`
}

// ResourceTarget describes one upstream resource the agent reaches
// through the sidecar. R-AN-PROXY-1 / R-AN-PROXY-2.
type ResourceTarget struct {
	// Name is a stable per-resource identifier (used in metrics).
	Name string `json:"name"`

	// +kubebuilder:validation:Enum=tcp;http
	Kind string `json:"kind"`

	// LocalAddr is the host:port the sidecar listens on (TCP).
	// +optional
	LocalAddr string `json:"localAddr,omitempty"`

	// LocalPort is the port the sidecar listens on (HTTP).
	// +optional
	LocalPort int32 `json:"localPort,omitempty"`

	// Gateway is the upstream the sidecar dials. For TCP: host:port.
	// For HTTP: a full URL.
	Gateway string `json:"gateway"`

	// Authorize lists SPIFFE authorizer descriptors (matched against
	// the gateway's peer SVID). At least one is required for TCP.
	// +optional
	Authorize []string `json:"authorize,omitempty"`

	// JWTAudience is the audience the HTTP sidecar should mint
	// JWT-SVIDs for. Required when kind=http.
	// +optional
	JWTAudience string `json:"jwtAudience,omitempty"`

	// TraT, when set, injects a Transaction Token (Txn-Token header) on
	// egress to this resource. http only. R-TRAT-API-1.
	// +optional
	TraT *TraTInjection `json:"trat,omitempty"`

	// Credential, when set, injects a broker-minted provider credential on
	// egress to this resource (the agent never sees it). http only. R-SEGR-API-1.
	// +optional
	Credential *CredentialInjection `json:"credential,omitempty"`
}

// EgressPolicy carries the eBPF-driven host policy — both transparent
// redirect of known CIDRs and a strict allow-list.
type EgressPolicy struct {
	// +optional
	// +kubebuilder:default:="ebpfBoth"
	// +kubebuilder:validation:Enum=none;ebpfRedirect;ebpfAllowList;ebpfBoth
	Enforcement string `json:"enforcement,omitempty"`

	// Allow is the per-(CIDR, ports, proto) allow-list. Anything
	// outside this list is dropped by the eBPF cgroup_skb/egress
	// program when Enforcement includes "ebpfAllowList".
	// +optional
	Allow []EgressRule `json:"allow,omitempty"`

	// RedirectCIDRs is the destination set the operator's
	// cgroup/connect4 program rewrites to point at the sidecar
	// (R-AN-EBPF-1). Tenants list the internal IP ranges they want
	// the sidecar to receive transparently.
	// +optional
	RedirectCIDRs []string `json:"redirectCIDRs,omitempty"`
}

// EgressRule is one entry in the allow-list.
type EgressRule struct {
	CIDR string `json:"cidr"`
	// Protocol: "tcp" | "udp" | "" (any).
	// +optional
	// +kubebuilder:validation:Enum=tcp;udp;""
	Protocol string `json:"protocol,omitempty"`
	// Ports is the list of allowed L4 ports. Empty list = any.
	// +optional
	Ports []int32 `json:"ports,omitempty"`
}

// WireGuardSpec configures the userspace WireGuard adapter.
type WireGuardSpec struct {
	// Mode: "client" (joins peers) | "server" (listens for peers).
	// +kubebuilder:validation:Enum=client;server
	Mode string `json:"mode"`

	// ListenPort is the UDP port the userspace device binds (server
	// mode). Default 51820.
	// +optional
	ListenPort int32 `json:"listenPort,omitempty"`

	// PrivateKeyRef points at a broker secret containing the
	// WireGuard private key (base64). MUST NOT be inlined.
	PrivateKeyRef AuthRef `json:"privateKeyRef"`

	// Addresses are the CIDRs assigned to the device (e.g. 10.99.0.5/32).
	// Required for server mode; recommended for client mode.
	// +optional
	Addresses []string `json:"addresses,omitempty"`

	// DNS is the resolver list applied to the netstack interface.
	// +optional
	DNS []string `json:"dns,omitempty"`

	// Peers is the static peer list.
	// +optional
	Peers []WGPeer `json:"peers,omitempty"`

	// MTU defaults to 1420 (WireGuard's default).
	// +optional
	MTU int32 `json:"mtu,omitempty"`
}

// WGPeer is one peer entry (for both client and server modes).
type WGPeer struct {
	// Name is a human label; the public key is the actual identity.
	Name string `json:"name"`

	// PublicKey is the peer's WireGuard public key (base64).
	PublicKey string `json:"publicKey"`

	// Endpoint is host:port, optional for server mode.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// AllowedIPs is the CIDR list that maps to this peer (the
	// "cryptokey routing" set).
	AllowedIPs []string `json:"allowedIPs"`

	// PersistentKeepalive in seconds (0 = disabled).
	// +optional
	PersistentKeepalive int32 `json:"persistentKeepalive,omitempty"`

	// PSKRef is an optional broker reference to a per-peer
	// pre-shared key.
	// +optional
	PSKRef *AuthRef `json:"pskRef,omitempty"`
}

// AgentNetworkStatus is reported by the controller.
type AgentNetworkStatus struct {
	Phase              string `json:"phase,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	BoundAgents        int32  `json:"boundAgents,omitempty"`
	WGPeerCount        int32  `json:"wgPeerCount,omitempty"`
	ProxyResourceCount int32  `json:"proxyResourceCount,omitempty"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
}

// ValidateAgentNetwork enforces R-AN-API-1 + transport-specific shape.
func ValidateAgentNetwork(s AgentNetworkSpec) error {
	var errs []error
	if !s.Kind.Valid() {
		errs = append(errs, fmt.Errorf("agentnetwork.kind=%q is invalid", s.Kind))
	}
	switch s.Kind {
	case NetworkIdentityProxy:
		if s.IdentityProxy == nil {
			errs = append(errs, errors.New("agentnetwork.identityProxy is required when kind=identityProxy"))
		} else {
			errs = append(errs, validateIdentityProxy(*s.IdentityProxy)...)
		}
		if s.WireGuardMesh != nil {
			errs = append(errs, errors.New("agentnetwork.wireguardMesh must be nil when kind=identityProxy"))
		}
	case NetworkWireGuardMesh:
		if s.WireGuardMesh == nil {
			errs = append(errs, errors.New("agentnetwork.wireguardMesh is required when kind=wireguardMesh"))
		} else {
			errs = append(errs, validateWireGuard(*s.WireGuardMesh)...)
		}
		if s.IdentityProxy != nil {
			errs = append(errs, errors.New("agentnetwork.identityProxy must be nil when kind=wireguardMesh"))
		}
	}
	return errors.Join(errs...)
}

// metadataNet is the link-local / instance-metadata range; an egress allow rule
// may never overlap it.
var metadataNet = func() *net.IPNet { _, n, _ := net.ParseCIDR("169.254.0.0/16"); return n }()

// metadataOverlap reports whether n intersects the metadata range — either n is
// inside it (e.g. 169.254.169.254/32) or n contains it (e.g. 0.0.0.0/0).
func metadataOverlap(n *net.IPNet) bool {
	if n == nil {
		return false
	}
	return metadataNet.Contains(n.IP) || n.Contains(metadataNet.IP)
}

func validateIdentityProxy(p IdentityProxySpec) []error {
	var errs []error
	if len(p.Resources) == 0 {
		errs = append(errs, errors.New("identityProxy.resources is empty"))
	}
	needsTTS, usesCred := false, false
	for i, r := range p.Resources {
		if r.Name == "" {
			errs = append(errs, fmt.Errorf("resources[%d].name is required", i))
		}
		switch r.Kind {
		case "tcp":
			if r.LocalAddr == "" {
				errs = append(errs, fmt.Errorf("resources[%d].localAddr is required (kind=tcp)", i))
			} else if _, _, err := net.SplitHostPort(r.LocalAddr); err != nil {
				errs = append(errs, fmt.Errorf("resources[%d].localAddr: %w", i, err))
			}
			if len(r.Authorize) == 0 {
				errs = append(errs, fmt.Errorf("resources[%d].authorize is required (kind=tcp) (R-MTL-1)", i))
			}
		case "http":
			if r.JWTAudience == "" {
				errs = append(errs, fmt.Errorf("resources[%d].jwtAudience is required (kind=http)", i))
			}
			if r.LocalPort <= 0 {
				errs = append(errs, fmt.Errorf("resources[%d].localPort is required (kind=http)", i))
			}
		default:
			errs = append(errs, fmt.Errorf("resources[%d].kind=%q invalid (want tcp|http)", i, r.Kind))
		}
		if r.Gateway == "" {
			errs = append(errs, fmt.Errorf("resources[%d].gateway is required", i))
		}
		if r.TraT != nil {
			needsTTS = true
			if r.Kind != "http" {
				errs = append(errs, fmt.Errorf("resources[%d].trat requires kind=http", i))
			}
			if r.TraT.Scope == "" {
				errs = append(errs, fmt.Errorf("resources[%d].trat.scope is required", i))
			}
		}
		if r.Credential != nil {
			needsTTS, usesCred = true, true
			if r.Kind != "http" {
				errs = append(errs, fmt.Errorf("resources[%d].credential requires kind=http", i))
			}
			if r.Credential.Name == "" {
				errs = append(errs, fmt.Errorf("resources[%d].credential.name is required", i))
			}
			if r.Credential.Scope == "" {
				errs = append(errs, fmt.Errorf("resources[%d].credential.scope is required", i))
			}
		}
	}
	if needsTTS && (p.TTS == nil || p.TTS.URL == "") {
		errs = append(errs, errors.New("identityProxy.tts.url is required when a resource sets trat or credential"))
	}
	if usesCred && (p.TTS == nil || p.TTS.JWKSURL == "") {
		errs = append(errs, errors.New("identityProxy.tts.jwksUrl is required when a resource sets credential (broker verifies the TraT)"))
	}
	for i, rule := range p.Egress.Allow {
		if _, ipnet, err := net.ParseCIDR(rule.CIDR); err != nil {
			errs = append(errs, fmt.Errorf("egress.allow[%d].cidr: %w", i, err))
		} else if metadataOverlap(ipnet) {
			// The instance-metadata range is the canonical SSRF / cloud-credential
			// exfil target; an allow rule may never open it (M1.19, D3).
			errs = append(errs, fmt.Errorf("egress.allow[%d].cidr %q overlaps the metadata range 169.254.0.0/16 (the egress floor is inviolable)", i, rule.CIDR))
		}
		if rule.Protocol != "" && rule.Protocol != "tcp" && rule.Protocol != "udp" {
			errs = append(errs, fmt.Errorf("egress.allow[%d].protocol=%q invalid", i, rule.Protocol))
		}
	}
	for i, c := range p.Egress.RedirectCIDRs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			errs = append(errs, fmt.Errorf("egress.redirectCIDRs[%d]: %w", i, err))
		}
	}
	switch p.Egress.Enforcement {
	case "", "none", "ebpfRedirect", "ebpfAllowList", "ebpfBoth":
	default:
		errs = append(errs, fmt.Errorf("egress.enforcement=%q invalid", p.Egress.Enforcement))
	}
	return errs
}

func validateWireGuard(w WireGuardSpec) []error {
	var errs []error
	if w.Mode != "client" && w.Mode != "server" {
		errs = append(errs, fmt.Errorf("wireguardMesh.mode=%q invalid (want client|server)", w.Mode))
	}
	if w.PrivateKeyRef.SecretName == "" {
		errs = append(errs, errors.New("wireguardMesh.privateKeyRef.secretName is required"))
	}
	if w.Mode == "server" {
		if w.ListenPort <= 0 {
			errs = append(errs, errors.New("wireguardMesh.listenPort is required (mode=server)"))
		}
		if len(w.Addresses) == 0 {
			errs = append(errs, errors.New("wireguardMesh.addresses is required (mode=server)"))
		}
	}
	if w.MTU != 0 && (w.MTU < 576 || w.MTU > 9000) {
		errs = append(errs, fmt.Errorf("wireguardMesh.mtu=%d outside [576,9000]", w.MTU))
	}
	for i, p := range w.Peers {
		if p.Name == "" {
			errs = append(errs, fmt.Errorf("peer[%d].name is required", i))
		}
		if !looksLikeBase64Key(p.PublicKey) {
			errs = append(errs, fmt.Errorf("peer[%d].publicKey is not a base64-encoded 32-byte key", i))
		}
		if len(p.AllowedIPs) == 0 {
			errs = append(errs, fmt.Errorf("peer[%d].allowedIPs is empty", i))
		}
		for j, c := range p.AllowedIPs {
			if _, _, err := net.ParseCIDR(c); err != nil {
				errs = append(errs, fmt.Errorf("peer[%d].allowedIPs[%d]: %w", i, j, err))
			}
		}
		if p.Endpoint != "" {
			if _, _, err := net.SplitHostPort(p.Endpoint); err != nil {
				errs = append(errs, fmt.Errorf("peer[%d].endpoint: %w", i, err))
			}
		}
		if p.PersistentKeepalive < 0 {
			errs = append(errs, fmt.Errorf("peer[%d].persistentKeepalive must be ≥ 0", i))
		}
	}
	for i, a := range w.Addresses {
		if _, _, err := net.ParseCIDR(a); err != nil {
			errs = append(errs, fmt.Errorf("addresses[%d]: %w", i, err))
		}
	}
	return errs
}

// looksLikeBase64Key returns true for strings shaped like a base64
// 32-byte WireGuard key — exactly 44 chars ending with '='.
func looksLikeBase64Key(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 44 || !strings.HasSuffix(s, "=") {
		return false
	}
	for _, r := range s[:len(s)-1] {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') || r == '+' || r == '/') {
			return false
		}
	}
	return true
}
