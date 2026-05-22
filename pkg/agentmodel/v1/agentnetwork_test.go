package v1

import (
	"strings"
	"testing"
)

func TestNetworkKind_Valid(t *testing.T) {
	for _, k := range []NetworkKind{NetworkIdentityProxy, NetworkWireGuardMesh} {
		if !k.Valid() {
			t.Errorf("%s should be valid", k)
		}
	}
	if NetworkKind("nope").Valid() {
		t.Error("unknown should be invalid")
	}
}

func validProxy() AgentNetworkSpec {
	return AgentNetworkSpec{
		Kind: NetworkIdentityProxy,
		IdentityProxy: &IdentityProxySpec{
			Resources: []ResourceTarget{
				{Name: "db", Kind: "tcp", LocalAddr: "127.0.0.1:5432",
					Gateway:   "pg-gw.infra:8443",
					Authorize: []string{"spiffe://smol-agents.ai/ns/infra/sa/pg"}},
				{Name: "billing", Kind: "http", LocalPort: 9100,
					Gateway: "https://billing.infra/", JWTAudience: "spiffe://smol-agents.ai/ns/infra/sa/billing"},
			},
			Egress: EgressPolicy{
				Enforcement:   "ebpfBoth",
				RedirectCIDRs: []string{"10.42.0.0/16"},
				Allow:         []EgressRule{{CIDR: "10.42.0.0/16", Protocol: "tcp", Ports: []int32{443, 5432}}},
			},
		},
	}
}

func validWG() AgentNetworkSpec {
	return AgentNetworkSpec{
		Kind: NetworkWireGuardMesh,
		WireGuardMesh: &WireGuardSpec{
			Mode:          "client",
			PrivateKeyRef: AuthRef{SecretName: "wg-private"},
			Addresses:     []string{"10.99.0.5/32"},
			Peers: []WGPeer{{
				Name:       "hub",
				PublicKey:  "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG=",
				Endpoint:   "vpn.example.com:51820",
				AllowedIPs: []string{"10.0.0.0/16"},
			}},
		},
	}
}

func TestValidate_ProxyHappy(t *testing.T) {
	if err := ValidateAgentNetwork(validProxy()); err != nil {
		t.Errorf("happy proxy: %v", err)
	}
}

func TestValidate_WGClientHappy(t *testing.T) {
	if err := ValidateAgentNetwork(validWG()); err != nil {
		t.Errorf("happy wg: %v", err)
	}
}

func TestValidate_WGServerRequiresAddresses(t *testing.T) {
	w := validWG()
	w.WireGuardMesh.Mode = "server"
	w.WireGuardMesh.ListenPort = 51820
	w.WireGuardMesh.Addresses = nil
	err := ValidateAgentNetwork(w)
	if err == nil || !strings.Contains(err.Error(), "addresses is required") {
		t.Errorf("expected addresses required: %v", err)
	}
}

func TestValidate_WGRequiresListenPortOnServer(t *testing.T) {
	w := validWG()
	w.WireGuardMesh.Mode = "server"
	w.WireGuardMesh.Addresses = []string{"10.99.0.1/32"}
	err := ValidateAgentNetwork(w)
	if err == nil || !strings.Contains(err.Error(), "listenPort") {
		t.Errorf("expected listenPort required: %v", err)
	}
}

func TestValidate_ProxyKindMutex(t *testing.T) {
	s := validProxy()
	s.WireGuardMesh = &WireGuardSpec{Mode: "client"}
	err := ValidateAgentNetwork(s)
	if err == nil {
		t.Error("expected mutex rejection")
	}
}

func TestValidate_ProxyResourceTCPRequiresAuthorize(t *testing.T) {
	s := validProxy()
	s.IdentityProxy.Resources[0].Authorize = nil
	err := ValidateAgentNetwork(s)
	if err == nil || !strings.Contains(err.Error(), "authorize is required") {
		t.Errorf("expected authorize required: %v", err)
	}
}

func TestValidate_ProxyResourceHTTPRequiresAudience(t *testing.T) {
	s := validProxy()
	s.IdentityProxy.Resources[1].JWTAudience = ""
	err := ValidateAgentNetwork(s)
	if err == nil || !strings.Contains(err.Error(), "jwtAudience is required") {
		t.Errorf("expected jwtAudience required: %v", err)
	}
}

func TestValidate_BadCIDR(t *testing.T) {
	s := validProxy()
	s.IdentityProxy.Egress.RedirectCIDRs = []string{"not-a-cidr"}
	err := ValidateAgentNetwork(s)
	if err == nil || !strings.Contains(err.Error(), "redirectCIDRs") {
		t.Errorf("expected cidr error: %v", err)
	}
}

func TestValidate_WGRejectsNonBase64Key(t *testing.T) {
	w := validWG()
	w.WireGuardMesh.Peers[0].PublicKey = "not-base64"
	err := ValidateAgentNetwork(w)
	if err == nil || !strings.Contains(err.Error(), "publicKey") {
		t.Errorf("expected publicKey error: %v", err)
	}
}

func TestValidate_WGMTUBounds(t *testing.T) {
	w := validWG()
	w.WireGuardMesh.MTU = 100
	err := ValidateAgentNetwork(w)
	if err == nil {
		t.Error("expected MTU error")
	}
}

func TestLooksLikeBase64Key(t *testing.T) {
	good := []string{
		"abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG=",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefg=",
	}
	for _, k := range good {
		if !looksLikeBase64Key(k) {
			t.Errorf("rejected good key %q", k)
		}
	}
	bad := []string{"", "short", strings.Repeat("a", 44), "not!valid$here....................==="}
	for _, k := range bad {
		if looksLikeBase64Key(k) {
			t.Errorf("accepted bad key %q", k)
		}
	}
}
