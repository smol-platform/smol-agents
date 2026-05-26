package builders

import (
	"testing"

	"gopkg.in/yaml.v3"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/secrets"
)

func TestAgentNeedsBroker(t *testing.T) {
	literal := &pure.AgentSpec{Harness: &pure.HarnessSpec{Env: []pure.HarnessEnvVar{{Name: "X", Value: "y"}}}}
	if AgentNeedsBroker(literal) {
		t.Error("literal-only env should not need the broker")
	}
	withSecret := &pure.AgentSpec{Harness: &pure.HarnessSpec{Env: []pure.HarnessEnvVar{
		{Name: "HEADER_Authorization", SecretRef: &pure.AuthRef{SecretName: "gw"}},
	}}}
	if !AgentNeedsBroker(withSecret) {
		t.Error("secretRef env should need the broker")
	}
}

func TestBuildAgentRunPod_AttachesBroker(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"
	agent := &amv1.Agent{}
	agent.Name = "a1"
	agent.Namespace = "tenant-a"
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{
		Kind: pure.HarnessHermes,
		HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"},
		Env:  []pure.HarnessEnvVar{{Name: "HEADER_Authorization", SecretRef: &pure.AuthRef{SecretName: "gw"}}},
	}

	pod := BuildAgentRunPod(run, agent)
	if !hasSidecar(pod, secretProxyName) {
		t.Error("secret-proxy sidecar not attached")
	}
	if !hasVolume(pod, secretBrokerVolumeName) || !hasVolume(pod, brokerConfigVolumeName) {
		t.Error("broker volumes missing")
	}
	// The execution container can dial the broker UDS.
	if _, ok := hasMount(pod.Spec.Containers[0], secretBrokerVolumeName); !ok {
		t.Error("execution container missing broker UDS mount")
	}
}

func TestBuildAgentRunPod_NoBrokerWithoutSecretRef(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "t"
	agent := &amv1.Agent{}
	agent.Name = "a1"
	agent.Namespace = "t"
	agent.Spec.Mode = pure.ModeHarness
	agent.Spec.Harness = &pure.HarnessSpec{Kind: pure.HarnessHermes, HTTP: &pure.HarnessHTTPSpec{URL: "http://gw"}}
	pod := BuildAgentRunPod(run, agent)
	if hasSidecar(pod, secretProxyName) {
		t.Error("no secretRef → no broker sidecar")
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
