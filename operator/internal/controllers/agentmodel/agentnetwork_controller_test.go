package agentmodel

import (
	"testing"

	amv1 "github.com/stigen/knative-agents/operator/api/agentmodel/v1"
	pure "github.com/stigen/knative-agents/pkg/agentmodel/v1"
)

func TestAgentNetworkSetStatus_RecordsAllFields(t *testing.T) {
	r := &AgentNetworkReconciler{}
	an := &amv1.AgentNetwork{}
	an.Generation = 7
	r.setStatus(an, "Pending", "SecretMissing", "wg-private not found")
	if an.Status.Phase != "Pending" {
		t.Errorf("phase = %q", an.Status.Phase)
	}
	if an.Status.Reason != "SecretMissing" {
		t.Errorf("reason = %q", an.Status.Reason)
	}
	if an.Status.Message != "wg-private not found" {
		t.Errorf("message = %q", an.Status.Message)
	}
	if an.Status.ObservedGeneration != 7 {
		t.Errorf("gen = %d", an.Status.ObservedGeneration)
	}
}

func TestAgentNetworkDeepCopy_PreservesContents(t *testing.T) {
	an := &amv1.AgentNetwork{}
	an.Spec.Kind = pure.NetworkIdentityProxy
	an.Spec.IdentityProxy = &pure.IdentityProxySpec{
		Resources: []pure.ResourceTarget{{
			Name: "db", Kind: "tcp", LocalAddr: "127.0.0.1:5432",
			Gateway: "pg.svc:8443", Authorize: []string{"spiffe://x"},
		}},
		Egress: pure.EgressPolicy{
			Enforcement:   "ebpfBoth",
			RedirectCIDRs: []string{"10.0.0.0/16"},
			Allow:         []pure.EgressRule{{CIDR: "10.0.0.5/32", Protocol: "tcp", Ports: []int32{443}}},
		},
	}
	cp := an.DeepCopy()
	if cp.Spec.IdentityProxy == nil {
		t.Fatal("identityProxy not copied")
	}
	cp.Spec.IdentityProxy.Resources[0].Name = "mutated"
	fresh := an.DeepCopy()
	if fresh.Spec.IdentityProxy.Resources[0].Name == "mutated" {
		t.Error("deepcopy shared the resources slice")
	}
}

func TestAgentNetworkDeepCopy_WireGuardBranch(t *testing.T) {
	an := &amv1.AgentNetwork{}
	an.Spec.Kind = pure.NetworkWireGuardMesh
	an.Spec.WireGuardMesh = &pure.WireGuardSpec{
		Mode:          "client",
		PrivateKeyRef: pure.AuthRef{SecretName: "wg-priv"},
		Peers: []pure.WGPeer{{
			Name:       "hub",
			PublicKey:  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			AllowedIPs: []string{"10.0.0.0/16"},
		}},
	}
	cp := an.DeepCopy()
	if cp.Spec.WireGuardMesh == nil {
		t.Fatal("wireguardMesh not copied")
	}
	cp.Spec.WireGuardMesh.Peers[0].Name = "mutated"
	fresh := an.DeepCopy()
	if fresh.Spec.WireGuardMesh.Peers[0].Name == "mutated" {
		t.Error("deepcopy shared the peers slice")
	}
}
