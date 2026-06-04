package plan

import (
	"testing"

	v1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func proxy(spec v1.IdentityProxySpec) v1.AgentNetworkSpec {
	return v1.AgentNetworkSpec{Kind: "identityProxy", IdentityProxy: &spec}
}

func TestBuildNetworkPlan_Empty(t *testing.T) {
	p, err := BuildNetworkPlan(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !p.Empty() || p.ProxyNeeded() || p.EbpfNeeded() {
		t.Fatalf("empty input must yield empty plan: %+v", p)
	}
	// wireguard / nil-proxy specs contribute nothing.
	p2, _ := BuildNetworkPlan([]v1.AgentNetworkSpec{{Kind: "wireguardMesh"}})
	if !p2.Empty() {
		t.Fatalf("wireguard spec should not populate the egress plan")
	}
}

func TestBuildNetworkPlan_UnionAndStrongestEnforcement(t *testing.T) {
	specs := []v1.AgentNetworkSpec{
		proxy(v1.IdentityProxySpec{
			Egress: v1.EgressPolicy{Enforcement: "ebpfAllowList", Allow: []v1.EgressRule{{CIDR: "10.0.0.0/8", Ports: []int32{443}}}},
		}),
		proxy(v1.IdentityProxySpec{
			Egress: v1.EgressPolicy{Enforcement: "ebpfRedirect", RedirectCIDRs: []string{"10.1.0.0/16"}},
		}),
	}
	p, err := BuildNetworkPlan(specs)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(p.AllowRules) != 1 || p.AllowRules[0].CIDR != "10.0.0.0/8" {
		t.Errorf("allow union wrong: %+v", p.AllowRules)
	}
	if len(p.RedirectCIDRs) != 1 {
		t.Errorf("redirect union wrong: %+v", p.RedirectCIDRs)
	}
	// allowList + redirect ⇒ ebpfBoth (strongest covering both facets).
	if p.Enforcement != "ebpfBoth" {
		t.Errorf("enforcement = %q, want ebpfBoth", p.Enforcement)
	}
	if !p.EbpfNeeded() {
		t.Errorf("ebpf should be needed")
	}
}

func TestBuildNetworkPlan_LocalPortConflict(t *testing.T) {
	specs := []v1.AgentNetworkSpec{
		proxy(v1.IdentityProxySpec{Resources: []v1.ResourceTarget{{Name: "a", Kind: "http", LocalPort: 8080, Gateway: "https://a"}}}),
		proxy(v1.IdentityProxySpec{Resources: []v1.ResourceTarget{{Name: "b", Kind: "http", LocalPort: 8080, Gateway: "https://b"}}}),
	}
	if _, err := BuildNetworkPlan(specs); err == nil {
		t.Fatalf("conflicting localPort must error")
	}
	// same port + same gateway is fine.
	ok := []v1.AgentNetworkSpec{
		proxy(v1.IdentityProxySpec{Resources: []v1.ResourceTarget{{Name: "a", Kind: "http", LocalPort: 8080, Gateway: "https://a"}}}),
		proxy(v1.IdentityProxySpec{Resources: []v1.ResourceTarget{{Name: "a2", Kind: "http", LocalPort: 8080, Gateway: "https://a"}}}),
	}
	if _, err := BuildNetworkPlan(ok); err != nil {
		t.Fatalf("same port+gateway should be fine: %v", err)
	}
}

func TestBuildNetworkPlan_TTSConflict(t *testing.T) {
	specs := []v1.AgentNetworkSpec{
		proxy(v1.IdentityProxySpec{TTS: &v1.TTSRef{URL: "https://tts-a"}}),
		proxy(v1.IdentityProxySpec{TTS: &v1.TTSRef{URL: "https://tts-b"}}),
	}
	if _, err := BuildNetworkPlan(specs); err == nil {
		t.Fatalf("conflicting TTS endpoints must error")
	}
	// identical TTS across networks composes to one.
	same := []v1.AgentNetworkSpec{
		proxy(v1.IdentityProxySpec{TTS: &v1.TTSRef{URL: "https://tts"}}),
		proxy(v1.IdentityProxySpec{TTS: &v1.TTSRef{URL: "https://tts"}}),
	}
	p, err := BuildNetworkPlan(same)
	if err != nil || p.TTS == nil || p.TTS.URL != "https://tts" {
		t.Fatalf("identical TTS should compose: %+v err=%v", p.TTS, err)
	}
}

func TestBuildNetworkPlan_RejectsMetadataCIDR(t *testing.T) {
	for _, cidr := range []string{"169.254.169.254/32", "169.254.0.0/16", "169.254.0.0/20"} {
		specs := []v1.AgentNetworkSpec{proxy(v1.IdentityProxySpec{
			Egress: v1.EgressPolicy{Enforcement: "ebpfAllowList", Allow: []v1.EgressRule{{CIDR: cidr}}},
		})}
		if _, err := BuildNetworkPlan(specs); err == nil {
			t.Errorf("allow CIDR %q overlapping metadata must be rejected", cidr)
		}
	}
	// a normal public CIDR is fine.
	ok := []v1.AgentNetworkSpec{proxy(v1.IdentityProxySpec{
		Egress: v1.EgressPolicy{Enforcement: "ebpfAllowList", Allow: []v1.EgressRule{{CIDR: "140.82.112.0/20"}}},
	})}
	if _, err := BuildNetworkPlan(ok); err != nil {
		t.Errorf("public CIDR should be allowed: %v", err)
	}
}
