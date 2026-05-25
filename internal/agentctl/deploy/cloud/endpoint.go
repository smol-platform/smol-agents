package cloud

import "fmt"

// APIEndpointInput is the subset of common options used to decide what the
// kubeconfig's server URL should be. Kept separate from the deploy package's
// option types so this package stays leaf-level (no import cycles).
type APIEndpointInput struct {
	CloudflareTunnelToken string
	APIHostname           string // required iff CloudflareTunnelToken is set
	TailscaleAuthKey      string // non-empty -> join tailnet; endpoint is the tailnet IP
	WireGuardConfig       string // path to wg-quick config; loaded here
}

// APIPlan is the preflight decision the deploy driver makes BEFORE provisioning.
// It carries the WG content to embed in cloud-init, the wg-side address (if
// known), and whether the SG/firewall must expose tcp/6443 publicly.
type APIPlan struct {
	Mode              string // "cloudflare" | "tailscale" | "wireguard" | "public"
	WGContent         string // wg0.conf body to embed in cloud-init (or "")
	WGAddress         string // [Interface] Address of the wg node (or "")
	APIHostname       string // copy of input for FinalizeServer
	ExposeAPIPublicly bool   // SG/firewall must open tcp/6443 (no tunnel/mesh)
}

// PlanAPIEndpoint reads the WG config (if any) and decides the preflight bits.
// Call before provisioning so the cloud-init user-data and the SG/firewall
// rules can be computed.
func PlanAPIEndpoint(in APIEndpointInput) (APIPlan, error) {
	var wgContent, wgAddr string
	if in.WireGuardConfig != "" {
		c, a, err := LoadWireGuardConfig(in.WireGuardConfig)
		if err != nil {
			return APIPlan{}, err
		}
		wgContent, wgAddr = c, a
	}
	// Precedence: cloudflare (explicit hostname, real TLS) > tailscale > wireguard
	// > public. The mesh/tunnel modes all keep tcp/6443 off the public internet.
	switch {
	case in.CloudflareTunnelToken != "":
		return APIPlan{
			Mode: "cloudflare", APIHostname: in.APIHostname,
			WGContent: wgContent, WGAddress: wgAddr,
			ExposeAPIPublicly: false,
		}, nil
	case in.TailscaleAuthKey != "":
		// The tailnet IP is assigned at join time; FinalizeServer takes it as
		// the addr resolved post-provision (see aws/hetzner Deploy).
		return APIPlan{
			Mode: "tailscale", WGContent: wgContent, WGAddress: wgAddr,
			ExposeAPIPublicly: false,
		}, nil
	case wgAddr != "":
		return APIPlan{
			Mode: "wireguard", WGContent: wgContent, WGAddress: wgAddr,
			ExposeAPIPublicly: false,
		}, nil
	default:
		return APIPlan{Mode: "public", ExposeAPIPublicly: true}, nil
	}
}

// FinalizeServer returns the kubeconfig server URL + InsecureSkipTLSVerify
// once the deploy knows the address the API is reachable at. addr is the
// address the caller resolved post-provision:
//   - mode=public:    the public IPv4 (required).
//   - mode=tailscale: the tailnet IPv4 read back over SSH (required).
//   - mode=cloudflare: ignored (uses APIHostname).
//   - mode=wireguard:  ignored (uses WGAddress from the config).
func (p APIPlan) FinalizeServer(addr string) (server string, skipVerify bool, err error) {
	switch p.Mode {
	case "cloudflare":
		return "https://" + p.APIHostname, false, nil
	case "wireguard":
		return "https://" + p.WGAddress + ":6443", true, nil
	case "tailscale":
		if addr == "" {
			return "", false, fmt.Errorf("FinalizeServer: tailnet addr required for mode=tailscale")
		}
		return "https://" + addr + ":6443", true, nil
	case "public":
		if addr == "" {
			return "", false, fmt.Errorf("FinalizeServer: publicIP required for mode=public")
		}
		return "https://" + addr + ":6443", true, nil
	}
	return "", false, fmt.Errorf("unknown plan mode %q", p.Mode)
}
