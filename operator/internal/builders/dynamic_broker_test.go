package builders

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

func sampleBackend() pure.DynamicCredentialBackendSpec {
	return pure.DynamicCredentialBackendSpec{
		CredentialName: "gh-token",
		Provider:       "githubApp",
		MaxLeaseTTL:    "5m",
		GitHubApp: &pure.GitHubAppBackendSpec{
			AppID:            "12345",
			PrivateKeyRef:    pure.AuthRef{SecretName: "gh-app-key", Key: "private-key.pem"},
			BaseURL:          "https://ghe.example.com/api/v3",
			ScopePermissions: map[string]map[string]string{"github:repo:read": {"contents": "read"}},
		},
		Grants: []pure.CredentialGrantSpec{
			{Principal: "spiffe://smol-agents.ai/ns/t/agent/writer", Scope: "github:repo:read", Repos: []string{"org/repo"}},
		},
	}
}

// The rendered config Secret must hold ONLY config.yaml and NO private-key bytes
// (the key is a separate volume) — the core of the agent-blind model.
func TestBuildDynamicBrokerConfigSecret_NoKeyBytes(t *testing.T) {
	sec, err := BuildDynamicBrokerConfigSecret("be", "t", sampleBackend(), "https://tts/jwks", "smol-agents.ai")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(sec.Data) != 1 {
		t.Fatalf("secret must hold exactly one key, got %v", keysOf(sec.Data))
	}
	if _, ok := sec.Data["config.yaml"]; !ok {
		t.Fatalf("secret must hold config.yaml, got %v", keysOf(sec.Data))
	}
	if sec.Name != "be-dynamic-broker" {
		t.Errorf("secret name = %q, want be-dynamic-broker", sec.Name)
	}
	body := string(sec.Data["config.yaml"])
	if strings.Contains(body, "PRIVATE KEY") || strings.Contains(strings.ToLower(body), "begin rsa") {
		t.Error("config must never contain key material")
	}

	// Parse it back and assert the dynamic mint shape.
	var cfg struct {
		PeerAuth string `yaml:"peerAuth"`
		// time.Duration with yaml.v3 is exactly how the secret-proxy parses it —
		// this asserts the maxLeaseTTL encoding round-trips (5m, not garbage).
		MaxLeaseTTL time.Duration `yaml:"maxLeaseTTL"`
		Backend     struct {
			Kind    string `yaml:"kind"`
			Dynamic struct {
				Provider       string `yaml:"provider"`
				AppID          string `yaml:"appID"`
				PrivateKeyPath string `yaml:"privateKeyPath"`
				BaseURL        string `yaml:"baseURL"`
			} `yaml:"dynamic"`
		} `yaml:"backend"`
		TTS struct {
			JWKSURL  string `yaml:"jwksURL"`
			Audience string `yaml:"audience"`
		} `yaml:"tts"`
		CredentialPolicy []struct {
			SPIFFEID   string   `yaml:"spiffeID"`
			Scope      string   `yaml:"scope"`
			Credential string   `yaml:"credential"`
			Repos      []string `yaml:"repos"`
		} `yaml:"credentialPolicy"`
	}
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf("rendered config is not valid yaml: %v", err)
	}
	if cfg.PeerAuth != "spire" {
		t.Errorf("peerAuth = %q, want spire (sender-constrained mint)", cfg.PeerAuth)
	}
	if cfg.MaxLeaseTTL != 5*time.Minute {
		t.Errorf("maxLeaseTTL round-trip = %v, want 5m (the secret-proxy parses this same field)", cfg.MaxLeaseTTL)
	}
	if cfg.Backend.Kind != "static" {
		t.Errorf("backend.kind = %q, want static (the secret-proxy always builds a backend)", cfg.Backend.Kind)
	}
	if cfg.Backend.Dynamic.AppID != "12345" || cfg.Backend.Dynamic.Provider != "githubApp" {
		t.Errorf("dynamic backend = %+v", cfg.Backend.Dynamic)
	}
	if cfg.Backend.Dynamic.PrivateKeyPath != DynamicBrokerKeyPath {
		t.Errorf("privateKeyPath = %q, want %q", cfg.Backend.Dynamic.PrivateKeyPath, DynamicBrokerKeyPath)
	}
	if cfg.TTS.JWKSURL != "https://tts/jwks" || cfg.TTS.Audience != "smol-agents.ai" {
		t.Errorf("tts = %+v", cfg.TTS)
	}
	if len(cfg.CredentialPolicy) != 1 || cfg.CredentialPolicy[0].Credential != "gh-token" ||
		cfg.CredentialPolicy[0].Scope != "github:repo:read" || cfg.CredentialPolicy[0].Repos[0] != "org/repo" {
		t.Errorf("credentialPolicy = %+v", cfg.CredentialPolicy)
	}
}

func TestBuildDynamicBrokerConfigSecret_RejectsNonGitHub(t *testing.T) {
	if _, err := BuildDynamicBrokerConfigSecret("be", "t", pure.DynamicCredentialBackendSpec{Provider: "vault"}, "j", "a"); err == nil {
		t.Fatal("non-githubApp provider must be rejected")
	}
}

// Agent-blindness: the root key + SPIRE socket mount ONLY into the broker sidecar;
// the agent container gets just the broker UDS (so it can request a mint, never
// read the key).
func TestAttachDynamicBroker_AgentBlind(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}}
	AttachDynamicBroker(pod, "be", pure.AuthRef{SecretName: "gh-app-key", Key: "private-key.pem"})

	// The agent (index 0) has the broker UDS — and NOT the key/SPIRE/config.
	agentMounts := mountSet(pod.Spec.Containers[0].VolumeMounts)
	if !agentMounts[dynamicBrokerVolumeName] {
		t.Error("agent must mount the broker UDS to request a mint")
	}
	for _, forbidden := range []string{dynamicBrokerKeyVolume, dynamicSpireVolumeName, dynamicBrokerConfigVolume} {
		if agentMounts[forbidden] {
			t.Errorf("agent container must NOT mount %q (agent-blind)", forbidden)
		}
	}

	// The broker sidecar is a native init container with key + SPIRE + config.
	var broker *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == dynamicSecretProxyName {
			broker = &pod.Spec.InitContainers[i]
		}
	}
	if broker == nil {
		t.Fatal("dynamic broker sidecar not added")
	}
	if broker.RestartPolicy == nil || *broker.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Error("broker must be a native sidecar (restartPolicy=Always) so a run pod still terminates")
	}
	brokerMounts := mountSet(broker.VolumeMounts)
	for _, need := range []string{dynamicBrokerKeyVolume, dynamicSpireVolumeName, dynamicBrokerConfigVolume, dynamicBrokerVolumeName} {
		if !brokerMounts[need] {
			t.Errorf("broker must mount %q", need)
		}
	}

	// The key volume projects privateKeyRef → the fixed in-broker filename.
	var keyVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == dynamicBrokerKeyVolume {
			keyVol = &pod.Spec.Volumes[i]
		}
	}
	if keyVol == nil || keyVol.Secret == nil {
		t.Fatal("key volume missing")
	}
	if keyVol.Secret.SecretName != "gh-app-key" {
		t.Errorf("key volume secret = %q, want gh-app-key", keyVol.Secret.SecretName)
	}
	if len(keyVol.Secret.Items) != 1 || keyVol.Secret.Items[0].Key != "private-key.pem" || keyVol.Secret.Items[0].Path != dynamicBrokerKeyFile {
		t.Errorf("key projection = %+v, want private-key.pem → %s", keyVol.Secret.Items, dynamicBrokerKeyFile)
	}
}

func TestAttachDynamicBroker_Idempotent(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}}
	AttachDynamicBroker(pod, "be", pure.AuthRef{SecretName: "k"})
	AttachDynamicBroker(pod, "be", pure.AuthRef{SecretName: "k"})
	n := 0
	for _, c := range pod.Spec.InitContainers {
		if c.Name == dynamicSecretProxyName {
			n++
		}
	}
	if n != 1 {
		t.Errorf("broker sidecar added %d times, want 1 (idempotent)", n)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mountSet(ms []corev1.VolumeMount) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		out[m.Name] = true
	}
	return out
}
