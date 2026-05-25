package cloud

import "fmt"

// APIEndpointInput is the subset of common options used to decide what the
// kubeconfig's server URL should be. Kept separate from the deploy package's
// option types so this package stays leaf-level (no import cycles).
type APIEndpointInput struct {
	CloudflareTunnelToken string
	APIHostname           string // required iff CloudflareTunnelToken is set
	WireGuardConfig       string // path to wg-quick config; loaded here
}

// APIPlan is the preflight decision the deploy driver makes BEFORE provisioning.
// It carries the WG content to embed in cloud-init, the wg-side address (if
// known), and whether the SG/firewall must expose tcp/6443 publicly.
type APIPlan struct {
	Mode              string // "cloudflare" | "wireguard" | "public"
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
	switch {
	case in.CloudflareTunnelToken != "":
		return APIPlan{
			Mode: "cloudflare", APIHostname: in.APIHostname,
			WGContent: wgContent, WGAddress: wgAddr,
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
// after the deploy knows the public IPv4. publicIP is required only when the
// plan is "public" (it's unused for the cloudflare + wireguard modes).
func (p APIPlan) FinalizeServer(publicIP string) (server string, skipVerify bool, err error) {
	switch p.Mode {
	case "cloudflare":
		return "https://" + p.APIHostname, false, nil
	case "wireguard":
		return "https://" + p.WGAddress + ":6443", true, nil
	case "public":
		if publicIP == "" {
			return "", false, fmt.Errorf("FinalizeServer: publicIP required for mode=public")
		}
		return "https://" + publicIP + ":6443", true, nil
	}
	return "", false, fmt.Errorf("unknown plan mode %q", p.Mode)
}
