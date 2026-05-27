package builders

import (
	"testing"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/secrets"
)

// findNativeSidecar returns the InitContainer with the given name and reports
// whether it is a properly-configured native sidecar (restartPolicy=Always),
// which is what makes the pod transition to a terminal phase when the main
// container exits.
func findNativeSidecar(pod *corev1.Pod, name string) (*corev1.Container, bool) {
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if c.Name != name {
			continue
		}
		isNative := c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
		return c, isNative
	}
	return nil, false
}

func TestAttachSecretBroker(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"
	agent := &amv1.Agent{}
	agent.Name = "a1"
	agent.Namespace = "tenant-a"
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessHermes, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}

	pod := BuildAgentRunPod(run, agent)
	// BuildAgentRunPod alone does not attach the broker — the controller drives it.
	if _, ok := findNativeSidecar(pod, secretProxyName); ok {
		t.Fatal("BuildAgentRunPod should not attach the broker")
	}

	AttachSecretBroker(pod, run.Name)

	// secret-proxy must be a native sidecar (InitContainer + restartPolicy=Always)
	// so k8s SIGTERMs it when the run container exits and the pod can reach a
	// terminal phase — without that, AgentRun.Status would stay stuck Running.
	c, native := findNativeSidecar(pod, secretProxyName)
	if c == nil {
		t.Fatal("secret-proxy not present in InitContainers")
	}
	if !native {
		t.Errorf("secret-proxy must be a native sidecar (restartPolicy=Always), got %v", c.RestartPolicy)
	}

	// It must be FIRST in InitContainers so it's running before any subsequent
	// regular init container (and well before main containers start).
	if pod.Spec.InitContainers[0].Name != secretProxyName {
		t.Errorf("secret-proxy must be the first InitContainer, got %q", pod.Spec.InitContainers[0].Name)
	}

	if !hasVolume(pod, secretBrokerVolumeName) || !hasVolume(pod, brokerConfigVolumeName) {
		t.Error("broker volumes missing")
	}
	if _, ok := hasMount(pod.Spec.Containers[0], secretBrokerVolumeName); !ok {
		t.Error("execution container missing broker UDS mount")
	}

	// Idempotent: a second call adds nothing.
	nInit := len(pod.Spec.InitContainers)
	nMain := len(pod.Spec.Containers)
	AttachSecretBroker(pod, run.Name)
	if len(pod.Spec.InitContainers) != nInit || len(pod.Spec.Containers) != nMain {
		t.Error("AttachSecretBroker not idempotent")
	}
}

func TestBuildBrokerConfigSecret(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"

	sec, err := BuildBrokerConfigSecret(run, map[string][]byte{"gw": []byte("Bearer s3cr3t")})
	if err != nil {
		t.Fatalf("BuildBrokerConfigSecret: %v", err)
	}
	if sec.Name != "r1-broker" {
		t.Errorf("name = %s", sec.Name)
	}

	var cfg struct {
		PeerAuth string `yaml:"peerAuth"`
		Backend  struct {
			Kind   string `yaml:"kind"`
			Static []struct {
				SPIFFEID string            `yaml:"spiffeID"`
				Items    map[string]string `yaml:"items"`
			} `yaml:"static"`
		} `yaml:"backend"`
		Policy []struct {
			SPIFFEID string   `yaml:"spiffeID"`
			Allow    []string `yaml:"allow"`
		} `yaml:"policy"`
	}
	if err := yaml.Unmarshal(sec.Data["config.yaml"], &cfg); err != nil {
		t.Fatalf("config.yaml does not parse: %v", err)
	}

	// The config must key on the SAME identity LocalPeerAttestor mints for the
	// run pod's uid — otherwise the broker would deny the agent.
	wantID, _ := secrets.LocalIDForUID("", uint32(RunPodUID))
	if cfg.PeerAuth != "local" {
		t.Errorf("peerAuth = %q, want local", cfg.PeerAuth)
	}
	if cfg.Backend.Kind != "static" || len(cfg.Backend.Static) != 1 {
		t.Fatalf("backend = %+v", cfg.Backend)
	}
	if cfg.Backend.Static[0].SPIFFEID != wantID.String() {
		t.Errorf("static spiffeID = %q, want %q", cfg.Backend.Static[0].SPIFFEID, wantID.String())
	}
	if got := cfg.Backend.Static[0].Items["gw"]; got != "Bearer s3cr3t" {
		t.Errorf("static value = %q", got)
	}
	if len(cfg.Policy) != 1 || cfg.Policy[0].SPIFFEID != wantID.String() ||
		len(cfg.Policy[0].Allow) != 1 || cfg.Policy[0].Allow[0] != "gw" {
		t.Errorf("policy = %+v", cfg.Policy)
	}
}
