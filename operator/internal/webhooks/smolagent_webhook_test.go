package webhooks

import (
	"errors"
	"strings"
	"testing"

	v1 "github.com/stigen/smol-agents/operator/api/v1"
	"github.com/stigen/smol-agents/operator/pkg/features"
)

func validCR() *v1.SmolAgent {
	cr := &v1.SmolAgent{}
	cr.Name = "alice"
	cr.Namespace = "tenant-a"
	cr.Spec.TrustDomain = "stigen.ai"
	cr.Spec.Features.Sandbox.Enabled = true
	cr.Spec.Features.Sandbox.RuntimeClass = "kata-fc"
	cr.Spec.Features.Identity.Mode = "strict"
	cr.Spec.Features.Identity.Enabled = true
	return cr
}

func TestValidateAgent_HappyPath(t *testing.T) {
	if err := ValidateAgent(validCR(), nil); err != nil {
		t.Errorf("happy: %v", err)
	}
}

func TestValidateAgent_RequiresTrustDomain(t *testing.T) {
	cr := validCR()
	cr.Spec.TrustDomain = ""
	err := ValidateAgent(cr, nil)
	if err == nil || !strings.Contains(err.Error(), "trustDomain") {
		t.Errorf("expected trustDomain error: %v", err)
	}
}

func TestValidateAgent_InsecureRequiresAnnotation(t *testing.T) {
	cr := validCR()
	cr.Spec.Mode = "insecure"
	err := ValidateAgent(cr, nil)
	if err == nil {
		t.Fatal("expected insecure error")
	}
	cr.Annotations = map[string]string{AllowInsecureAnnotation: "true"}
	if err := ValidateAgent(cr, nil); err != nil {
		t.Errorf("with annotation: %v", err)
	}
}

func TestValidateAgent_RuncRequiresAllowHostEscape(t *testing.T) {
	cr := validCR()
	cr.Spec.Features.Sandbox.RuntimeClass = "runc"
	err := ValidateAgent(cr, nil)
	if err == nil || !strings.Contains(err.Error(), "allowHostEscape") {
		t.Errorf("expected runc/allowHostEscape error: %v", err)
	}
	cr.Spec.Features.Sandbox.AllowHostEscape = true
	if err := ValidateAgent(cr, nil); err != nil {
		t.Errorf("with AllowHostEscape: %v", err)
	}
}

func TestValidateAgent_ForbiddenFeatureRejected(t *testing.T) {
	cr := validCR()
	cr.Spec.Features.EBPF.Enabled = true
	platform := &v1.SmolAgentPlatform{}
	platform.Spec.FeaturePolicy = []v1.FeaturePolicyRow{
		{Feature: string(features.EBPF), Allowed: false},
	}
	err := ValidateAgent(cr, platform)
	if err == nil || !strings.Contains(err.Error(), "ebpf") {
		t.Errorf("expected ebpf forbidden: %v", err)
	}
}

func TestValidateAgent_AllowedFeaturePasses(t *testing.T) {
	cr := validCR()
	cr.Spec.Features.EBPF.Enabled = true
	platform := &v1.SmolAgentPlatform{}
	platform.Spec.FeaturePolicy = []v1.FeaturePolicyRow{
		{Feature: string(features.EBPF), Allowed: true},
	}
	if err := ValidateAgent(cr, platform); err != nil {
		t.Errorf("allowed feature rejected: %v", err)
	}
}

func TestDefaultAgent_FillsAllUnsetFields(t *testing.T) {
	cr := &v1.SmolAgent{}
	platform := &v1.SmolAgentPlatform{}
	platform.Spec.DefaultTrustDomain = "stigen.ai"
	DefaultAgent(cr, platform)
	if cr.Spec.TrustDomain != "stigen.ai" {
		t.Errorf("trustDomain default: %q", cr.Spec.TrustDomain)
	}
	if cr.Spec.DeploymentKind != "knative" {
		t.Errorf("deploymentKind default: %q", cr.Spec.DeploymentKind)
	}
	if cr.Spec.Replicas != 1 {
		t.Errorf("replicas default: %d", cr.Spec.Replicas)
	}
	if cr.Spec.Features.Sandbox.RuntimeClass != "kata-fc" {
		t.Errorf("sandbox default: %q", cr.Spec.Features.Sandbox.RuntimeClass)
	}
	if cr.Spec.Features.Identity.Mode != "strict" {
		t.Errorf("identity.mode default: %q", cr.Spec.Features.Identity.Mode)
	}
}

func TestDefaultAgent_DoesNotOverrideExplicit(t *testing.T) {
	cr := validCR()
	cr.Spec.Features.Sandbox.RuntimeClass = "gvisor"
	cr.Spec.DeploymentKind = "deployment"
	DefaultAgent(cr, nil)
	if cr.Spec.Features.Sandbox.RuntimeClass != "gvisor" {
		t.Errorf("explicit value overwritten")
	}
	if cr.Spec.DeploymentKind != "deployment" {
		t.Errorf("explicit value overwritten")
	}
}

func TestValidatePlatformSingleton_NameEnforced(t *testing.T) {
	p := &v1.SmolAgentPlatform{}
	p.Name = "wrong-name"
	p.Spec.DefaultTrustDomain = "stigen.ai"
	if err := ValidatePlatformSingleton(p, "default"); err == nil {
		t.Error("expected non-singleton rejection")
	}
	p.Name = "default"
	if err := ValidatePlatformSingleton(p, "default"); err != nil {
		t.Errorf("singleton rejected: %v", err)
	}
}

func TestValidatePlatformSingleton_PolicyFeatureMustExist(t *testing.T) {
	p := &v1.SmolAgentPlatform{}
	p.Name = "default"
	p.Spec.DefaultTrustDomain = "stigen.ai"
	p.Spec.FeaturePolicy = []v1.FeaturePolicyRow{{Feature: "garbage", Allowed: true}}
	if err := ValidatePlatformSingleton(p, "default"); err == nil {
		t.Error("expected unknown-feature rejection")
	}
}

func TestValidateAgent_JoinedErrors(t *testing.T) {
	cr := &v1.SmolAgent{}
	cr.Spec.Mode = "insecure"
	cr.Spec.Features.Sandbox.RuntimeClass = "runc"
	err := ValidateAgent(cr, nil)
	if err == nil {
		t.Fatal("expected errors")
	}
	// errors.Join wraps multiple lines
	wrapped := errors.Unwrap(err)
	_ = wrapped
}
