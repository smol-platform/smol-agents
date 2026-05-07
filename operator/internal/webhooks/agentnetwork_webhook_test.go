package webhooks

import (
	"context"
	"strings"
	"testing"

	amv1 "github.com/stigen/knative-agents/operator/api/agentmodel/v1"
	pure "github.com/stigen/knative-agents/pkg/agentmodel/v1"
)

func TestAgentNetworkWebhook_RejectsBothTransports(t *testing.T) {
	w := &agentNetworkWebhook{}
	an := &amv1.AgentNetwork{
		Spec: pure.AgentNetworkSpec{
			Kind: pure.NetworkIdentityProxy,
			IdentityProxy: &pure.IdentityProxySpec{
				Resources: []pure.ResourceTarget{{
					Name: "x", Kind: "tcp",
					LocalAddr: "127.0.0.1:5432",
					Gateway:   "g.svc:8443",
					Authorize: []string{"spiffe://x"},
				}},
			},
			WireGuardMesh: &pure.WireGuardSpec{
				Mode:          "client",
				PrivateKeyRef: pure.AuthRef{SecretName: "x"},
			},
		},
	}
	_, err := w.ValidateCreate(context.Background(), an)
	if err == nil {
		t.Fatal("expected rejection for both-transports spec")
	}
	if !strings.Contains(err.Error(), "wireguardMesh must be nil") {
		t.Errorf("error message doesn't mention transport mutex: %v", err)
	}
}

func TestAgentNetworkWebhook_AcceptsValidProxy(t *testing.T) {
	w := &agentNetworkWebhook{}
	an := &amv1.AgentNetwork{
		Spec: pure.AgentNetworkSpec{
			Kind: pure.NetworkIdentityProxy,
			IdentityProxy: &pure.IdentityProxySpec{
				Resources: []pure.ResourceTarget{{
					Name: "x", Kind: "tcp",
					LocalAddr: "127.0.0.1:5432",
					Gateway:   "g.svc:8443",
					Authorize: []string{"spiffe://x"},
				}},
			},
		},
	}
	if _, err := w.ValidateCreate(context.Background(), an); err != nil {
		t.Errorf("valid proxy spec rejected: %v", err)
	}
}

func TestAgentNetworkWebhook_RejectsBadKind(t *testing.T) {
	w := &agentNetworkWebhook{}
	an := &amv1.AgentNetwork{Spec: pure.AgentNetworkSpec{Kind: "junk"}}
	_, err := w.ValidateCreate(context.Background(), an)
	if err == nil {
		t.Fatal("expected rejection for invalid kind")
	}
}

func TestAgentNetworkWebhook_DeleteAlwaysAllowed(t *testing.T) {
	w := &agentNetworkWebhook{}
	if _, err := w.ValidateDelete(context.Background(), &amv1.AgentNetwork{}); err != nil {
		t.Errorf("delete should always be allowed: %v", err)
	}
}
