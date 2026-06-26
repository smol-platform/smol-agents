package builders

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

	// The gateway container drops ALL caps but adds back the ones s6-overlay needs
	// to drop root → hermes(10000) (proven on gtr: without these it crash-loops).
	sc := c.SecurityContext
	if sc == nil || sc.Capabilities == nil {
		t.Fatalf("gateway container has no capability hardening")
	}
	if len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("gateway caps.drop = %v, want [ALL]", sc.Capabilities.Drop)
	}
	want := map[corev1.Capability]bool{"SETUID": false, "SETGID": false, "CHOWN": false}
	for _, a := range sc.Capabilities.Add {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for cap, found := range want {
		if !found {
			t.Errorf("gateway caps.add missing %s (s6 privilege-drop)", cap)
		}
	}
	if sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation {
		t.Errorf("gateway allowPrivilegeEscalation = %v, want true (s6 step-down)", sc.AllowPrivilegeEscalation)
	}
	// The busybox config-seed init stays fully locked (drop ALL, no add-back).
	ic := dep.Spec.Template.Spec.InitContainers[0].SecurityContext
	if ic == nil || ic.Capabilities == nil || len(ic.Capabilities.Add) != 0 {
		t.Errorf("init container should add no caps, got %+v", ic)
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
	// Without UI exposure: only the gateway port, no UI service/endpoint.
	if len(np.Spec.Ingress[0].Ports) != 1 {
		t.Errorf("ingress should allow only the gateway port when UI off, got %d", len(np.Spec.Ingress[0].Ports))
	}
	if got := ModelGatewayUIEndpoint(gw); got != "" {
		t.Errorf("UI endpoint should be empty when UI off, got %q", got)
	}
	if got := ModelGatewayEndpoint(gw); got != "http://mgw-hermes.tenant-a.svc:8642" {
		t.Errorf("endpoint = %q", got)
	}
}

func sampleGatewayWithUI() *amv1.ModelGateway {
	gw := sampleGateway()
	gw.Spec.UI = &pure.GatewayUISpec{
		Expose: true,
		Auth:   pure.GatewayUIAuth{Mode: "sharedSecret", SecretRef: &pure.AuthRef{SecretName: "hermes-ui-htpasswd"}},
	}
	return gw
}

func TestBuildModelGatewayUI(t *testing.T) {
	gw := sampleGatewayWithUI()

	// ConfigMap carries the rendered nginx server block.
	cm := BuildModelGatewayConfigMap(gw)
	conf := cm.Data["ui-nginx.conf"]
	if conf == "" || !strings.Contains(conf, "listen 8643;") || !strings.Contains(conf, "proxy_pass http://127.0.0.1:8642;") {
		t.Errorf("ui-nginx.conf missing/incorrect: %q", conf)
	}
	if !strings.Contains(conf, "auth_basic_user_file /etc/nginx/auth/.htpasswd;") {
		t.Errorf("ui-nginx.conf should enforce basic-auth: %q", conf)
	}

	// Deployment gains the hardened auth sidecar + its two volumes.
	dep := BuildModelGatewayDeployment(gw, "kata-fc")
	var side *corev1.Container
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == "ui-auth" {
			side = &dep.Spec.Template.Spec.Containers[i]
		}
	}
	if side == nil {
		t.Fatalf("expected a ui-auth sidecar, containers=%+v", dep.Spec.Template.Spec.Containers)
	}
	if side.Ports[0].ContainerPort != 8643 {
		t.Errorf("ui sidecar port = %d, want 8643", side.Ports[0].ContainerPort)
	}
	if sc := side.SecurityContext; sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("ui sidecar should be fully hardened (drop ALL, no add, no privesc): %+v", side.SecurityContext)
	}
	vols := map[string]bool{}
	for _, v := range dep.Spec.Template.Spec.Volumes {
		vols[v.Name] = true
	}
	if !vols["ui-config"] || !vols["ui-auth"] {
		t.Errorf("ui volumes missing: %v", vols)
	}

	// Dedicated UI Service + endpoint.
	uisvc := BuildModelGatewayUIService(gw)
	if uisvc.Name != "mgw-hermes-ui" || uisvc.Spec.Ports[0].Port != 8643 {
		t.Errorf("ui service name/port = %s/%d", uisvc.Name, uisvc.Spec.Ports[0].Port)
	}
	if got := ModelGatewayUIEndpoint(gw); got != "http://mgw-hermes-ui.tenant-a.svc:8643" {
		t.Errorf("ui endpoint = %q", got)
	}

	// Ingress now allows the UI port too.
	np := BuildModelGatewayIngress(gw)
	if len(np.Spec.Ingress[0].Ports) != 2 {
		t.Errorf("ingress should allow gateway + UI ports, got %d", len(np.Spec.Ingress[0].Ports))
	}
}
