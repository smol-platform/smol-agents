package builders

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func sampleGateway() *amv1.ModelGateway {
	return &amv1.ModelGateway{
		ObjectMeta: metav1.ObjectMeta{Name: "hermes", Namespace: "tenant-a"},
		Spec: pure.ModelGatewaySpec{
			Provider: "hermes",
			Image:    "nousresearch/hermes-agent:latest",
			Config:   "model:\n  provider: zai\n  model: glm-4.6\n",
			Env: []pure.HarnessEnvVar{
				{Name: "GLM_API_KEY", SecretRef: &pure.AuthRef{SecretName: "zai-key", Key: "ZAI_API_KEY"}},
				{Name: "GLM_BASE_URL", Value: "https://api.z.ai/api/coding/paas/v4"},
				{Name: "API_SERVER_KEY", SecretRef: &pure.AuthRef{SecretName: "hermes-gw-key"}}, // key defaults to name
			},
		},
	}
}

func TestBuildModelGatewayConfigMap(t *testing.T) {
	cm := BuildModelGatewayConfigMap(sampleGateway())
	if cm.Name != "mgw-hermes" || cm.Namespace != "tenant-a" {
		t.Fatalf("cm name/ns = %s/%s", cm.Name, cm.Namespace)
	}
	if got := cm.Data["config.yaml"]; got == "" || got[:5] != "model" {
		t.Errorf("config.yaml = %q", got)
	}
}

func TestBuildModelGatewayDeployment(t *testing.T) {
	gw := sampleGateway()
	dep := BuildModelGatewayDeployment(gw, "kata-fc")

	// RuntimeClass applied (RCE → microVM).
	if dep.Spec.Template.Spec.RuntimeClassName == nil || *dep.Spec.Template.Spec.RuntimeClassName != "kata-fc" {
		t.Errorf("runtimeClass = %v, want kata-fc", dep.Spec.Template.Spec.RuntimeClassName)
	}
	// init container seeds the config.
	if len(dep.Spec.Template.Spec.InitContainers) != 1 || dep.Spec.Template.Spec.InitContainers[0].Name != "seed-config" {
		t.Fatalf("want a seed-config init container, got %+v", dep.Spec.Template.Spec.InitContainers)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	if c.Image != gw.Spec.Image {
		t.Errorf("image = %q", c.Image)
	}
	if len(c.Args) != 2 || c.Args[0] != "gateway" || c.Args[1] != "run" {
		t.Errorf("args = %v, want [gateway run]", c.Args)
	}
	if c.Ports[0].ContainerPort != pure.HermesDefaultPort {
		t.Errorf("port = %d, want %d", c.Ports[0].ContainerPort, pure.HermesDefaultPort)
	}
	// env: provider conventions + user env (secretRef → secretKeyRef, key default = name).
	env := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			env[e.Name] = "secret:" + e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
		} else {
			env[e.Name] = e.Value
		}
	}
	if env["API_SERVER_ENABLED"] != "true" || env["API_SERVER_HOST"] != "0.0.0.0" {
		t.Errorf("missing hermes std env: %v", env)
	}
	if env["GLM_API_KEY"] != "secret:zai-key/ZAI_API_KEY" {
		t.Errorf("GLM_API_KEY env = %q", env["GLM_API_KEY"])
	}
	if env["API_SERVER_KEY"] != "secret:hermes-gw-key/API_SERVER_KEY" { // key defaulted to name
		t.Errorf("API_SERVER_KEY env = %q (key should default to the env name)", env["API_SERVER_KEY"])
	}
	if env["GLM_BASE_URL"] != "https://api.z.ai/api/coding/paas/v4" {
		t.Errorf("GLM_BASE_URL = %q", env["GLM_BASE_URL"])
	}
}

func TestBuildModelGatewayDeployment_NoRuntimeClassForRunc(t *testing.T) {
	for _, class := range []string{"", "runc"} {
		dep := BuildModelGatewayDeployment(sampleGateway(), class)
		if dep.Spec.Template.Spec.RuntimeClassName != nil {
			t.Errorf("class=%q should leave RuntimeClassName unset, got %v", class, *dep.Spec.Template.Spec.RuntimeClassName)
		}
	}
}

func TestBuildModelGatewayServiceAndIngress(t *testing.T) {
	gw := sampleGateway()
	svc := BuildModelGatewayService(gw)
	if svc.Spec.Ports[0].Port != pure.HermesDefaultPort {
		t.Errorf("service port = %d", svc.Spec.Ports[0].Port)
	}
	np := BuildModelGatewayIngress(gw)
	if np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "tenant-a" {
		t.Errorf("ingress should be confined to the gateway namespace: %+v", np.Spec.Ingress)
	}
	if got := ModelGatewayEndpoint(gw); got != "http://mgw-hermes.tenant-a.svc:8642" {
		t.Errorf("endpoint = %q", got)
	}
}
