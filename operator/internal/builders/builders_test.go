package builders

import (
	"strings"
	"testing"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

func sample() *v1.SmolAgent {
	cr := &v1.SmolAgent{}
	cr.Name = "alice"
	cr.Namespace = "tenant-a"
	cr.Spec.TrustDomain = "smol-agents.ai"
	cr.Spec.Mode = "strict"
	cr.Spec.Features.Identity.Enabled = true
	cr.Spec.Features.Transport.Private.Enabled = true
	cr.Spec.Features.Secrets.Enabled = true
	cr.Spec.Features.EBPF.Enabled = true
	cr.Spec.Features.Sandbox.Enabled = true
	cr.Spec.Features.Sandbox.RuntimeClass = "kata-fc"
	cr.Spec.Features.Observability.Enabled = true
	return cr
}

func TestLabels_Stable(t *testing.T) {
	cr := sample()
	a := Labels(cr)
	b := Labels(cr)
	if len(a) != len(b) {
		t.Fatal("labels not stable")
	}
	for k, v := range a {
		if b[k] != v {
			t.Errorf("label %q drifted: %q vs %q", k, v, b[k])
		}
	}
}

func TestBuildAgentConfigMap_ContainsAllSections(t *testing.T) {
	cm := BuildAgentConfigMap(sample())
	if cm.Name != "alice-config" || cm.Namespace != "tenant-a" {
		t.Errorf("name/namespace wrong: %s/%s", cm.Namespace, cm.Name)
	}
	yaml := cm.Data["agent.yaml"]
	for _, want := range []string{
		"mode: strict",
		"trustDomain: smol-agents.ai",
		"identity:",
		"transport:",
		"private:",
		"secrets:",
		"ebpf:",
		"sandbox:",
		"runtimeClass: kata-fc",
		"observability:",
		"runtime:",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("agent.yaml missing %q\n---\n%s", want, yaml)
		}
	}
}

func TestBuildAgentConfigMap_DisabledFeatures_OmitSections(t *testing.T) {
	cr := sample()
	cr.Spec.Features.EBPF.Enabled = false
	cr.Spec.Features.Secrets.Enabled = false
	yaml := BuildAgentConfigMap(cr).Data["agent.yaml"]
	if strings.Contains(yaml, "ebpf:") || strings.Contains(yaml, "secrets:") {
		t.Errorf("disabled features leaked into yaml:\n%s", yaml)
	}
}

func TestBuildAgentConfigMap_PublicTransport_RequiresPaths(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Transport.Public.Enabled = true
	cr.Spec.Features.Transport.Public.CertPath = "/tls/c"
	cr.Spec.Features.Transport.Public.KeyPath = "/tls/k"
	yaml := BuildAgentConfigMap(cr).Data["agent.yaml"]
	if !strings.Contains(yaml, "/tls/c") {
		t.Errorf("certPath missing:\n%s", yaml)
	}
}

func TestBuildClusterSPIFFEID(t *testing.T) {
	c := BuildClusterSPIFFEID(sample())
	if c.GetName() != "tenant-a-alice" {
		t.Errorf("name = %s", c.GetName())
	}
	spec, ok := c.Object["spec"].(map[string]any)
	if !ok {
		t.Fatal("missing spec")
	}
	tmpl := spec["spiffeIDTemplate"].(string)
	if !strings.HasPrefix(tmpl, "spiffe://smol-agents.ai/") {
		t.Errorf("spiffeIDTemplate = %q", tmpl)
	}
}

func TestBuildServiceAccount(t *testing.T) {
	sa := BuildServiceAccount(sample())
	if sa.Name != "alice-agent" {
		t.Errorf("name = %s", sa.Name)
	}
}

func TestBuildRuntimeClassKataFC(t *testing.T) {
	rc := BuildRuntimeClassKataFC()
	if rc.Name != "kata-fc" || rc.Handler != "kata-fc" {
		t.Errorf("got %+v", rc)
	}
}

func TestBuildRuntimeClassGVisor(t *testing.T) {
	rc := BuildRuntimeClassGVisor()
	if rc.Name != "gvisor" || rc.Handler != "runsc" {
		t.Errorf("got %+v", rc)
	}
}

func TestNonEmpty(t *testing.T) {
	if nonEmpty("", "x") != "x" || nonEmpty("y", "x") != "y" {
		t.Error("nonEmpty wrong")
	}
}
