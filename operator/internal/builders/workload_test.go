package builders

import (
	"testing"
)

func TestBuildAgentPodSpec_DefaultRuntimeClass(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Sandbox.RuntimeClass = ""
	pod := BuildAgentPodSpec(cr)
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "kata-fc" {
		t.Errorf("default runtimeClassName = %v, want kata-fc", pod.RuntimeClassName)
	}
}

func TestBuildAgentPodSpec_SecretsToggle(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Secrets.Enabled = true
	pod := BuildAgentPodSpec(cr)
	if len(pod.Containers) != 2 {
		t.Errorf("with secrets enabled: containers=%d, want 2", len(pod.Containers))
	}
	cr.Spec.Features.Secrets.Enabled = false
	pod = BuildAgentPodSpec(cr)
	if len(pod.Containers) != 1 {
		t.Errorf("with secrets disabled: containers=%d, want 1", len(pod.Containers))
	}
}

func TestBuildAgentPodSpec_AlwaysHardenedSecurityContext(t *testing.T) {
	pod := BuildAgentPodSpec(sample())
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("PodSecurityContext must runAsNonRoot")
	}
	for _, c := range pod.Containers {
		if c.SecurityContext == nil {
			t.Errorf("container %s missing SecurityContext", c.Name)
			continue
		}
		if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
			t.Errorf("container %s allows privilege escalation", c.Name)
		}
		if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
			t.Errorf("container %s does not have readOnlyRootFilesystem", c.Name)
		}
	}
}

func TestBuildDeployment(t *testing.T) {
	cr := sample()
	cr.Spec.Replicas = 3
	dep := BuildDeployment(cr)
	if dep.Name != cr.Name || dep.Namespace != cr.Namespace {
		t.Errorf("name/ns wrong")
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 3 {
		t.Errorf("replicas = %v", dep.Spec.Replicas)
	}
	if dep.Spec.Selector.MatchLabels["app.kubernetes.io/instance"] != cr.Name {
		t.Errorf("selector wrong")
	}
}

func TestBuildStatefulSet(t *testing.T) {
	cr := sample()
	ss := BuildStatefulSet(cr)
	if ss.Spec.ServiceName != cr.Name {
		t.Errorf("serviceName = %q", ss.Spec.ServiceName)
	}
	if len(ss.Spec.VolumeClaimTemplates) != 1 || ss.Spec.VolumeClaimTemplates[0].Name != "state" {
		t.Errorf("VCT wrong: %+v", ss.Spec.VolumeClaimTemplates)
	}
}

func TestBuildKnativeService(t *testing.T) {
	cr := sample()
	cr.Spec.Features.Knative.MinScale = 0
	cr.Spec.Features.Knative.MaxScale = 10
	svc := BuildKnativeService(cr)
	if svc.GetKind() != "Service" || svc.GroupVersionKind().Group != "serving.knative.dev" {
		t.Errorf("GVK = %s", svc.GroupVersionKind())
	}
	spec, _ := svc.Object["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	meta, _ := tmpl["metadata"].(map[string]any)
	annot, _ := meta["annotations"].(map[string]any)
	if annot["autoscaling.knative.dev/max-scale"] != "10" {
		t.Errorf("max-scale annotation = %v", annot["autoscaling.knative.dev/max-scale"])
	}
	tspec, _ := tmpl["spec"].(map[string]any)
	if tspec["runtimeClassName"] != "kata-fc" {
		t.Errorf("runtimeClassName = %v", tspec["runtimeClassName"])
	}
}

func TestAgentImage_Override(t *testing.T) {
	cr := sample()
	if AgentImage(cr) != "smol-agents/agent:0.1.0" {
		t.Errorf("default image wrong: %q", AgentImage(cr))
	}
	cr.Spec.Image = "myreg.example.com/agent:custom"
	if AgentImage(cr) != "myreg.example.com/agent:custom" {
		t.Errorf("override ignored: %q", AgentImage(cr))
	}
}
