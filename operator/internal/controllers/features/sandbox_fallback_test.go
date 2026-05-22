package features

import (
	"context"
	"testing"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

func kataSandboxAgent() *v1.SmolAgent {
	cr := &v1.SmolAgent{}
	cr.Name = "a"
	cr.Spec.Features.Sandbox.Enabled = true
	cr.Spec.Features.Sandbox.RuntimeClass = "kata-fc"
	return cr
}

func platformWithFallback(allow bool) *v1.SmolAgentPlatform {
	p := &v1.SmolAgentPlatform{}
	p.Spec.NodeProvisioning.AllowGvisorFallback = allow
	return p
}

func TestSandbox_FallsBackToGvisorWhenNoPoolAndAllowed(t *testing.T) {
	cr := kataSandboxAgent()
	env := Env{CR: cr, Platform: platformWithFallback(true), Reader: stubReader{}}
	res, _, err := SandboxReconciler{}.Reconcile(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cr.Spec.Features.Sandbox.RuntimeClass != "gvisor" {
		t.Errorf("expected fallback to gvisor, runtimeClass=%q", cr.Spec.Features.Sandbox.RuntimeClass)
	}
	if res.Mode != "gvisor" {
		t.Errorf("res.Mode = %q, want gvisor", res.Mode)
	}
}

func TestSandbox_NoKVMCapacityWhenFallbackDisabled(t *testing.T) {
	cr := kataSandboxAgent()
	env := Env{CR: cr, Platform: platformWithFallback(false), Reader: stubReader{}}
	res, _, err := SandboxReconciler{}.Reconcile(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when no kata pool and fallback disabled")
	}
	if res.Reason != "NoKVMCapacity" {
		t.Errorf("res.Reason = %q, want NoKVMCapacity", res.Reason)
	}
	if cr.Spec.Features.Sandbox.RuntimeClass != "kata-fc" {
		t.Errorf("runtimeClass should be unchanged on failure, got %q", cr.Spec.Features.Sandbox.RuntimeClass)
	}
}

func TestSandbox_KeepsKataWhenPoolExists(t *testing.T) {
	cr := kataSandboxAgent()
	env := Env{
		CR:       cr,
		Platform: platformWithFallback(false),
		Reader:   stubReader{pools: []v1.AgentNodePool{pool("kata-arm64", "kata-fc")}},
	}
	_, _, err := SandboxReconciler{}.Reconcile(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if cr.Spec.Features.Sandbox.RuntimeClass != "kata-fc" {
		t.Errorf("runtimeClass should stay kata-fc when a pool exists, got %q", cr.Spec.Features.Sandbox.RuntimeClass)
	}
}
