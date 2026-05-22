package features

import (
	"context"
	"testing"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
	"github.com/smol-platform/smol-agents/operator/pkg/features"
)

func sample() *v1.SmolAgent {
	cr := &v1.SmolAgent{}
	cr.Name = "alice"
	cr.Namespace = "tenant-a"
	cr.Spec.TrustDomain = "smol-agents.ai"
	cr.Spec.Features.Identity.Enabled = true
	cr.Spec.Features.Sandbox.Enabled = true
	cr.Spec.Features.Sandbox.RuntimeClass = "kata-fc"
	cr.Spec.Features.Secrets.Enabled = true
	cr.Spec.Features.EBPF.Enabled = true
	return cr
}

func samplePlatform() *v1.SmolAgentPlatform {
	p := &v1.SmolAgentPlatform{}
	p.Spec.EBPFLoader.Enabled = true
	return p
}

func TestIdentityReconciler_HappyPath(t *testing.T) {
	r := IdentityReconciler{}
	res, owned, err := r.Reconcile(context.Background(), Env{CR: sample()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready {
		t.Errorf("expected Ready, reason=%s", res.Reason)
	}
	if len(owned) != 3 {
		t.Errorf("expected 3 owned objects, got %d", len(owned))
	}
}

func TestIdentityReconciler_DisabledNoObjects(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Identity.Enabled = false
	res, owned, _ := IdentityReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if len(owned) != 0 || res.Ready {
		t.Errorf("disabled feature produced state: ready=%v owned=%d", res.Ready, len(owned))
	}
	if res.Reason != "Disabled" {
		t.Errorf("reason=%q want Disabled", res.Reason)
	}
}

func TestIdentityReconciler_MissingTrustDomain(t *testing.T) {
	cr := sample()
	cr.Spec.TrustDomain = ""
	res, _, _ := IdentityReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if res.Ready || res.Reason != "PrerequisitesUnmet" {
		t.Errorf("expected PrerequisitesUnmet, got %+v", res)
	}
}

func TestSandboxReconciler_RuncWithoutEscape_Errors(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Sandbox.RuntimeClass = "runc"
	cr.Spec.Features.Sandbox.AllowHostEscape = false
	res, _, err := SandboxReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if err == nil {
		t.Fatal("expected error for runc without escape")
	}
	if res.Reason != "PolicyViolation" {
		t.Errorf("reason=%s", res.Reason)
	}
}

func TestSandboxReconciler_GVisor_NoReader_AssumesPresent(t *testing.T) {
	res, owned, err := SandboxReconciler{}.Reconcile(context.Background(), Env{CR: sample()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready {
		t.Errorf("expected Ready, reason=%s", res.Reason)
	}
	if len(owned) != 0 {
		t.Errorf("without Reader we should not synthesise RuntimeClass")
	}
}

func TestEBPFReconciler_NoPlatform(t *testing.T) {
	res, _, _ := EBPFReconciler{}.Reconcile(context.Background(), Env{CR: sample()})
	if res.Ready || res.Reason != "PrerequisitesUnmet" {
		t.Errorf("expected PrerequisitesUnmet, got %+v", res)
	}
}

func TestEBPFReconciler_LoaderDisabled(t *testing.T) {
	p := samplePlatform()
	p.Spec.EBPFLoader.Enabled = false
	res, _, _ := EBPFReconciler{}.Reconcile(context.Background(), Env{CR: sample(), Platform: p})
	if res.Ready || res.Reason != "PrerequisitesUnmet" {
		t.Errorf("expected PrerequisitesUnmet, got %+v", res)
	}
}

func TestEBPFReconciler_HappyPath(t *testing.T) {
	res, _, err := EBPFReconciler{}.Reconcile(context.Background(), Env{CR: sample(), Platform: samplePlatform()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ready {
		t.Errorf("expected Ready, reason=%s", res.Reason)
	}
}

func TestSecretsReconciler_HappyAndDisabled(t *testing.T) {
	res, _, _ := SecretsReconciler{}.Reconcile(context.Background(), Env{CR: sample()})
	if !res.Ready {
		t.Errorf("happy: not ready, reason=%s", res.Reason)
	}
	cr := sample()
	cr.Spec.Features.Secrets.Enabled = false
	res, _, _ = SecretsReconciler{}.Reconcile(context.Background(), Env{CR: cr})
	if res.Ready || res.Reason != "Disabled" {
		t.Errorf("disabled: %+v", res)
	}
}

func TestRegistryHasAllReconcilers(t *testing.T) {
	all := []FeatureReconciler{
		IdentityReconciler{},
		SandboxReconciler{},
		SecretsReconciler{},
		EBPFReconciler{},
	}
	seen := map[features.Feature]struct{}{}
	for _, r := range all {
		seen[r.Name()] = struct{}{}
	}
	for _, want := range []features.Feature{features.Identity, features.Sandbox, features.Secrets, features.EBPF} {
		if _, ok := seen[want]; !ok {
			t.Errorf("missing reconciler for %s", want)
		}
	}
}
