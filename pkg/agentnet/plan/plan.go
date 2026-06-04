// Package plan composes one or more AgentNetwork specs that bind to a single
// agent into a single NetworkPlan — the pure value the operator's datapath
// builders (NetworkPolicy, eBPF map writer, sidecar injector) consume. It has
// no Kubernetes dependency beyond the pure agentmodel types.
package plan

import (
	"fmt"
	"net/netip"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

// linkLocal is the instance-metadata range. An allow rule may never open it —
// the metadata floor is inviolable (SSRF to 169.254.169.254 is the classic
// cloud-credential exfil). BuildNetworkPlan rejects any allow CIDR overlapping
// it rather than silently dropping the rule.
var linkLocal = netip.MustParsePrefix("169.254.0.0/16")

// NetworkPlan is the AND-composition of every AgentNetwork bound to an agent.
type NetworkPlan struct {
	// AllowRules is the union of every identityProxy egress allow-list.
	AllowRules []v1.EgressRule
	// RedirectCIDRs is the union of every transparent-redirect CIDR.
	RedirectCIDRs []string
	// ProxyResources is the concatenation of every identityProxy resource.
	ProxyResources []v1.ResourceTarget
	// TTS is the (single, consistent) token service across the bound networks.
	TTS *v1.TTSRef
	// Enforcement is the strongest eBPF tier any bound network requested.
	Enforcement string
	// Networks is the names of the bound AgentNetworks; populated by the caller
	// (the pure compositor takes specs, which carry no name).
	Networks []string
}

// BuildNetworkPlan AND-composes identityProxy specs (wireguard and nil-proxy
// specs contribute nothing to the egress plan). It unions allow/redirect lists,
// concatenates proxy resources, takes a single consistent TTS, and selects the
// strongest enforcement tier. It errors on a cross-network conflict: two
// resources binding the same local port/addr to different gateways, conflicting
// TTS endpoints, or an allow rule that would open the metadata range.
func BuildNetworkPlan(specs []v1.AgentNetworkSpec) (NetworkPlan, error) {
	var p NetworkPlan
	var wantAllow, wantRedirect bool
	byPort := map[int32]string{}  // localPort -> gateway
	byAddr := map[string]string{} // localAddr -> gateway

	for _, s := range specs {
		ip := s.IdentityProxy
		if ip == nil {
			continue
		}
		for _, r := range ip.Egress.Allow {
			if err := rejectMetadata(r.CIDR); err != nil {
				return NetworkPlan{}, err
			}
			p.AllowRules = append(p.AllowRules, r)
		}
		p.RedirectCIDRs = append(p.RedirectCIDRs, ip.Egress.RedirectCIDRs...)

		for _, res := range ip.Resources {
			if res.LocalPort != 0 {
				if gw, ok := byPort[res.LocalPort]; ok && gw != res.Gateway {
					return NetworkPlan{}, fmt.Errorf("network conflict: localPort %d bound to both %q and %q", res.LocalPort, gw, res.Gateway)
				}
				byPort[res.LocalPort] = res.Gateway
			}
			if res.LocalAddr != "" {
				if gw, ok := byAddr[res.LocalAddr]; ok && gw != res.Gateway {
					return NetworkPlan{}, fmt.Errorf("network conflict: localAddr %q bound to both %q and %q", res.LocalAddr, gw, res.Gateway)
				}
				byAddr[res.LocalAddr] = res.Gateway
			}
			p.ProxyResources = append(p.ProxyResources, res)
		}

		if ip.TTS != nil {
			if p.TTS != nil && p.TTS.URL != ip.TTS.URL {
				return NetworkPlan{}, fmt.Errorf("network conflict: conflicting TTS endpoints %q and %q", p.TTS.URL, ip.TTS.URL)
			}
			if p.TTS == nil {
				p.TTS = ip.TTS
			}
		}

		switch ip.Egress.Enforcement {
		case "ebpfAllowList":
			wantAllow = true
		case "ebpfRedirect":
			wantRedirect = true
		case "ebpfBoth":
			wantAllow, wantRedirect = true, true
		}
	}

	p.Enforcement = composeEnforcement(wantAllow, wantRedirect)
	return p, nil
}

func composeEnforcement(allow, redirect bool) string {
	switch {
	case allow && redirect:
		return "ebpfBoth"
	case allow:
		return "ebpfAllowList"
	case redirect:
		return "ebpfRedirect"
	default:
		return "none"
	}
}

// rejectMetadata errors if cidr overlaps the instance-metadata link-local range.
func rejectMetadata(cidr string) error {
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil {
		// Bare address → treat as /32 (or /128).
		addr, aerr := netip.ParseAddr(cidr)
		if aerr != nil {
			return fmt.Errorf("invalid allow cidr %q", cidr)
		}
		pfx = netip.PrefixFrom(addr, addr.BitLen())
	}
	if linkLocal.Overlaps(pfx) {
		return fmt.Errorf("allow cidr %q overlaps the metadata range %s — the egress floor is inviolable", cidr, linkLocal)
	}
	return nil
}

// ProxyNeeded reports whether any bound network needs the identity-proxy sidecar
// (it declares upstream resources).
func (p NetworkPlan) ProxyNeeded() bool { return len(p.ProxyResources) > 0 }

// EbpfNeeded reports whether the datapath needs the eBPF tier (any enforcement
// beyond "none"/"").
func (p NetworkPlan) EbpfNeeded() bool {
	return p.Enforcement != "" && p.Enforcement != "none"
}

// Empty reports whether nothing was composed (no bound identityProxy network).
func (p NetworkPlan) Empty() bool {
	return len(p.AllowRules) == 0 && len(p.RedirectCIDRs) == 0 &&
		len(p.ProxyResources) == 0 && p.TTS == nil && !p.EbpfNeeded()
}
