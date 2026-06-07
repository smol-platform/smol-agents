package builders

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

func terminalCR(tf v1.TerminalFeature) *v1.SmolAgent {
	cr := &v1.SmolAgent{ObjectMeta: metav1.ObjectMeta{Name: "claude", Namespace: "tenant-a"}}
	cr.Spec.Features.Terminal = tf
	return cr
}

func containerByName(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

// M4.8: a terminal-disabled agent gets no ttyd sidecars and its agent container
// entrypoint is untouched.
func TestWireTerminal_DisabledNoOp(t *testing.T) {
	spec := BuildAgentPodSpec(terminalCR(v1.TerminalFeature{}))
	for _, c := range spec.Containers {
		if strings.HasPrefix(c.Name, "ttyd") {
			t.Fatalf("terminal disabled but found sidecar %q", c.Name)
		}
	}
	if hasVolumeNamed(spec.Volumes, "tmux-sock") {
		t.Error("terminal disabled but tmux-sock volume present")
	}
}

// M4.8: enabling terminal wraps the agent in tmux "main" and adds a writable
// driver ttyd + a read-only viewer ttyd, both hardened and gateway-gated.
func TestWireTerminal_DriverAndViewer(t *testing.T) {
	cr := terminalCR(v1.TerminalFeature{
		FeatureBase: v1.FeatureBase{Enabled: true}, Multiplex: true, ReadOnlyDefault: true,
	})
	spec := BuildAgentPodSpec(cr)

	// Agent wrapped in tmux.
	agent := containerByName(spec.Containers, "agent")
	if agent == nil {
		t.Fatal("agent container missing")
	}
	boot := strings.Join(append(agent.Command, agent.Args...), " ")
	if !strings.Contains(boot, "tmux") || !strings.Contains(boot, "new-session") || !strings.Contains(boot, tmuxSocketPath) {
		t.Errorf("agent not wrapped in tmux: %q", boot)
	}
	if !hasVolumeNamed(spec.Volumes, "tmux-sock") {
		t.Error("tmux-sock volume missing")
	}

	driver := containerByName(spec.Containers, "ttyd-driver")
	viewer := containerByName(spec.Containers, "ttyd-viewer")
	if driver == nil || viewer == nil {
		t.Fatalf("driver/viewer ttyd missing: driver=%v viewer=%v", driver != nil, viewer != nil)
	}
	dargs := strings.Join(driver.Args, " ")
	vargs := strings.Join(viewer.Args, " ")
	// Driver is writable; viewer is NOT, and attaches tmux read-only (-r).
	if !strings.Contains(dargs, "-W") {
		t.Errorf("driver must be writable (-W): %q", dargs)
	}
	if strings.Contains(vargs, "-W") {
		t.Errorf("viewer must NOT be writable: %q", vargs)
	}
	if !strings.Contains(vargs, "attach -t "+tmuxSessionName+" -r") {
		t.Errorf("viewer must attach tmux read-only (-r): %q", vargs)
	}
	// Both: origin-checked, gateway auth header required, correct ports.
	for _, c := range []*corev1.Container{driver, viewer} {
		a := strings.Join(c.Args, " ")
		if !strings.Contains(a, "-O") || !strings.Contains(a, "--auth-header "+TerminalAuthHeader) {
			t.Errorf("%s missing origin-check / auth-header: %q", c.Name, a)
		}
		// Hardened PSA, like the secret-proxy.
		sc := c.SecurityContext
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
			sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
			sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("%s not hardened: %+v", c.Name, sc)
		}
		if len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("%s must drop ALL caps", c.Name)
		}
	}
	if driver.Ports[0].ContainerPort != TerminalDriverPort || viewer.Ports[0].ContainerPort != TerminalViewerPort {
		t.Errorf("ports = driver:%d viewer:%d", driver.Ports[0].ContainerPort, viewer.Ports[0].ContainerPort)
	}
}

// M4.8: a non-multiplex terminal still gets a ttyd but does NOT rewrite the
// agent entrypoint (the agent isn't forced into tmux).
func TestWireTerminal_NoMultiplexKeepsEntrypoint(t *testing.T) {
	cr := terminalCR(v1.TerminalFeature{FeatureBase: v1.FeatureBase{Enabled: true}, Multiplex: false})
	spec := BuildAgentPodSpec(cr)
	agent := containerByName(spec.Containers, "agent")
	if strings.Contains(strings.Join(agent.Command, " "), "tmux") {
		t.Errorf("non-multiplex must not wrap the agent in tmux: %v", agent.Command)
	}
	if containerByName(spec.Containers, "ttyd-driver") == nil {
		t.Error("driver ttyd should still be present")
	}
}

// M4.9: the terminal Service is ClusterIP, selects the agent's pods, and fronts
// both ttyd ports; the ingress NetworkPolicy admits those ports only from the
// gateway's namespace+pod selector.
func TestBuildAgentTerminalServiceAndIngress(t *testing.T) {
	cr := terminalCR(v1.TerminalFeature{FeatureBase: v1.FeatureBase{Enabled: true}})
	svc := BuildAgentTerminalService(cr)
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("service type = %s, want ClusterIP", svc.Spec.Type)
	}
	if len(svc.Spec.Ports) != 2 || svc.Spec.Ports[0].Port != TerminalDriverPort || svc.Spec.Ports[1].Port != TerminalViewerPort {
		t.Errorf("service ports = %+v", svc.Spec.Ports)
	}
	for k, v := range Selector(cr) {
		if svc.Spec.Selector[k] != v {
			t.Errorf("service selector missing %s=%s", k, v)
		}
	}

	np := BuildAgentTerminalIngress(cr, "smol-system", map[string]string{"app.kubernetes.io/component": "agentterminal"})
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != "Ingress" {
		t.Errorf("policyTypes = %v, want [Ingress]", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].From) != 1 {
		t.Fatalf("ingress rules = %+v", np.Spec.Ingress)
	}
	peer := np.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "smol-system" {
		t.Errorf("gateway namespace selector = %+v", peer.NamespaceSelector)
	}
	if peer.PodSelector.MatchLabels["app.kubernetes.io/component"] != "agentterminal" {
		t.Errorf("gateway pod selector = %+v", peer.PodSelector)
	}
	if len(np.Spec.Ingress[0].Ports) != 2 {
		t.Errorf("ingress must scope to the 2 ttyd ports, got %+v", np.Spec.Ingress[0].Ports)
	}
}

// M4.11: the recorder sidecar lands only when record AND an AgentFS volume is
// present (it has nowhere durable to ship a cast otherwise).
func TestWireTerminal_RecorderNeedsAgentFS(t *testing.T) {
	cr := terminalCR(v1.TerminalFeature{FeatureBase: v1.FeatureBase{Enabled: true}, Record: true})
	// No AgentFS volume on the base serving pod → no recorder.
	spec := BuildAgentPodSpec(cr)
	if containerByName(spec.Containers, "ttyd-recorder") != nil {
		t.Error("recorder present without an AgentFS volume")
	}
	// With an AgentFS volume, the recorder appears and writes under the workspace.
	spec.Volumes = append(spec.Volumes, corev1.Volume{Name: storageFSVolumeName})
	WireTerminal(&spec, cr)
	rec := containerByName(spec.Containers, "ttyd-recorder")
	if rec == nil {
		t.Fatal("recorder missing with AgentFS present")
	}
	if !strings.Contains(strings.Join(rec.Args, " "), defaultStorageMountPath) {
		t.Errorf("recorder must write under the workspace: %v", rec.Args)
	}
}
