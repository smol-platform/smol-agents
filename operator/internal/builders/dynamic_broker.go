// Package builders — dynamic_broker.go
//
// The PRODUCER half of the dynamic provider-credential mint path (D8, M1.23): it
// renders the SPIRE-backed secret-proxy config for a DynamicCredentialBackend and
// attaches that broker (config + root key + SPIRE socket) to a pod. The root key
// (a GitHub App private key) is mounted ONLY into the broker sidecar — the agent
// container never sees it; the agent requests a short-lived, TraT-authorized,
// request-scoped credential over the broker UDS and the broker mints it. This is
// the agent-blind credential model: the agent holds no provider secret.
//
// CONSUMER (the controller wiring that resolves a bound AgentNetwork credential to
// its DynamicCredentialBackend, creates the config Secret, and calls
// AttachDynamicBroker on the run/session pod, plus the end-to-end mint over the
// UDS) lands with the M2 proxy+SPIRE-broker injection. This file is producer-only.
package builders

import (
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
)

const (
	dynamicBrokerSocketDir    = "/run/dynamic-broker"
	dynamicBrokerVolumeName   = "dynamic-broker"
	dynamicBrokerConfigVolume = "dynamic-broker-config"
	dynamicBrokerKeyVolume    = "dynamic-broker-key"
	dynamicBrokerKeyDir       = "/etc/dynamic-broker-key"
	dynamicBrokerKeyFile      = "key.pem"
	dynamicSecretProxyName    = "dynamic-secret-proxy"
	dynamicSpireVolumeName    = "spire-agent-socket"
	dynamicSpireDir           = "/run/spire/agent-sockets"

	// DynamicBrokerKeyPath is the IN-BROKER path the root key mounts at; the
	// rendered config's backend.dynamic.privateKeyPath points here, and
	// AttachDynamicBroker projects the key Secret to this exact file. The agent
	// container never mounts this volume.
	DynamicBrokerKeyPath = dynamicBrokerKeyDir + "/" + dynamicBrokerKeyFile
	// DynamicBrokerWorkloadAPI is the SPIRE workload-API socket the broker uses to
	// fetch its SVID (peerAuth=spire), matching the secret-proxy default.
	DynamicBrokerWorkloadAPI = "unix://" + dynamicSpireDir + "/api.sock"
	// DynamicBrokerSocketPath is the broker UDS the agent dials to request a mint.
	DynamicBrokerSocketPath = dynamicBrokerSocketDir + "/dynamic-broker.sock"
)

// DynamicBrokerConfigSecretName is the config Secret name for a backend.
func DynamicBrokerConfigSecretName(backendName string) string {
	return backendName + "-dynamic-broker"
}

// dynamicBrokerConfigYAML marshals to cmd/secret-proxy's brokerConfig dialect
// (yaml tags must match its parser). The static backend is declared (kind:
// static, no items) because the secret-proxy always builds a backend; the dynamic
// block adds the mint path ALONGSIDE it.
type dynamicBrokerConfigYAML struct {
	SocketPath      string        `yaml:"socketPath"`
	WorkloadAPIAddr string        `yaml:"workloadAPI"`
	PeerAuth        string        `yaml:"peerAuth"`
	MaxLeaseTTL     time.Duration `yaml:"maxLeaseTTL,omitempty"`
	Backend         struct {
		Kind    string                   `yaml:"kind"`
		Dynamic dynamicBrokerBackendYAML `yaml:"dynamic"`
	} `yaml:"backend"`
	TTS              dynamicBrokerTTSYAML      `yaml:"tts"`
	CredentialPolicy []dynamicBrokerPolicyYAML `yaml:"credentialPolicy"`
}

type dynamicBrokerBackendYAML struct {
	Provider         string                       `yaml:"provider"`
	AppID            string                       `yaml:"appID"`
	PrivateKeyPath   string                       `yaml:"privateKeyPath"`
	BaseURL          string                       `yaml:"baseURL,omitempty"`
	ScopePermissions map[string]map[string]string `yaml:"scopePermissions,omitempty"`
}

type dynamicBrokerTTSYAML struct {
	JWKSURL  string `yaml:"jwksURL"`
	Audience string `yaml:"audience"`
}

type dynamicBrokerPolicyYAML struct {
	SPIFFEID   string   `yaml:"spiffeID"`
	Scope      string   `yaml:"scope"`
	Credential string   `yaml:"credential"`
	Repos      []string `yaml:"repos,omitempty"`
}

// BuildDynamicBrokerConfigSecret renders the SPIRE-backed broker's secret-proxy
// config for a DynamicCredentialBackend: the github-app mint backend (appID + the
// IN-BROKER path of the root key — never the key bytes), the TraT verifier (TTS
// JWKS + audience, from the bound AgentNetwork), and the deny-by-default
// credentialPolicy built from the backend's grants. peerAuth is spire so the mint
// is sender-constrained (the TraT is bound to the caller's SVID).
//
// The returned Secret holds ONLY config.yaml — NO private key. The key is a
// SEPARATE volume that AttachDynamicBroker mounts into the broker alone, so this
// Secret is safe even though it is referenced from a pod spec.
func BuildDynamicBrokerConfigSecret(name, namespace string, dcb pure.DynamicCredentialBackendSpec, ttsJWKSURL, ttsAudience string) (*corev1.Secret, error) {
	if dcb.Provider != "githubApp" || dcb.GitHubApp == nil {
		return nil, fmt.Errorf("dynamic broker: only provider=githubApp is supported (got %q)", dcb.Provider)
	}
	var maxTTL time.Duration
	if dcb.MaxLeaseTTL != "" {
		d, err := time.ParseDuration(dcb.MaxLeaseTTL)
		if err != nil {
			return nil, fmt.Errorf("dynamic broker: maxLeaseTTL %q: %w", dcb.MaxLeaseTTL, err)
		}
		maxTTL = d
	}

	var cfg dynamicBrokerConfigYAML
	cfg.SocketPath = DynamicBrokerSocketPath
	cfg.WorkloadAPIAddr = DynamicBrokerWorkloadAPI
	cfg.PeerAuth = "spire" // REQUIRED for the dynamic mint path (sender-constraint)
	cfg.MaxLeaseTTL = maxTTL
	cfg.Backend.Kind = "static" // empty static backend; the dynamic block adds the mint path
	cfg.Backend.Dynamic = dynamicBrokerBackendYAML{
		Provider:         dcb.Provider,
		AppID:            dcb.GitHubApp.AppID,
		PrivateKeyPath:   DynamicBrokerKeyPath,
		BaseURL:          dcb.GitHubApp.BaseURL,
		ScopePermissions: dcb.GitHubApp.ScopePermissions,
	}
	cfg.TTS = dynamicBrokerTTSYAML{JWKSURL: ttsJWKSURL, Audience: ttsAudience}

	// One credentialPolicy entry per grant: principal may mint this backend's
	// credential at the granted scope, optionally constrained to repos. Sorted for
	// a deterministic render.
	pols := make([]dynamicBrokerPolicyYAML, 0, len(dcb.Grants))
	for _, g := range dcb.Grants {
		pols = append(pols, dynamicBrokerPolicyYAML{
			SPIFFEID:   g.Principal,
			Scope:      g.Scope,
			Credential: dcb.CredentialName,
			Repos:      g.Repos,
		})
	}
	sort.Slice(pols, func(i, j int) bool {
		if pols[i].SPIFFEID != pols[j].SPIFFEID {
			return pols[i].SPIFFEID < pols[j].SPIFFEID
		}
		return pols[i].Scope < pols[j].Scope
	})
	cfg.CredentialPolicy = pols

	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal dynamic broker config: %w", err)
	}
	return &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      DynamicBrokerConfigSecretName(name),
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "smol-agents",
				"app.kubernetes.io/component": "dynamic-broker",
			},
		},
		Data: map[string][]byte{"config.yaml": raw},
	}, nil
}

// AttachDynamicBroker adds the SPIRE-backed dynamic-mint broker to a pod: the
// broker UDS (shared with the agent), the config Secret, the root-key volume, and
// the SPIRE CSI socket. The KEY and SPIRE socket are mounted ONLY into the broker
// sidecar — the agent container (index 0) gets just the broker UDS, so it can
// REQUEST a mint but never reads the root key (agent-blind, D8). backendName keys
// the config Secret (DynamicBrokerConfigSecretName); privateKeyRef is the root
// key Secret. Idempotent. The sidecar is native (init + restartPolicy=Always) so
// a run pod still terminates when the agent exits.
func AttachDynamicBroker(pod *corev1.Pod, backendName string, privateKeyRef pure.AuthRef) *corev1.Pod {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return pod
	}
	for _, c := range pod.Spec.InitContainers {
		if c.Name == dynamicSecretProxyName {
			return pod
		}
	}

	// The source key in the root Secret projected to the fixed broker path; an
	// unset Key follows the "key.pem" convention.
	srcKey := privateKeyRef.Key
	if srcKey == "" {
		srcKey = dynamicBrokerKeyFile
	}

	pod.Spec.Volumes = append(pod.Spec.Volumes,
		corev1.Volume{Name: dynamicBrokerVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		corev1.Volume{Name: dynamicBrokerConfigVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: DynamicBrokerConfigSecretName(backendName)},
		}},
		corev1.Volume{Name: dynamicBrokerKeyVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: privateKeyRef.SecretName,
				Items:      []corev1.KeyToPath{{Key: srcKey, Path: dynamicBrokerKeyFile}},
			},
		}},
	)
	if !hasVolumeNamed(pod.Spec.Volumes, dynamicSpireVolumeName) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name:         dynamicSpireVolumeName,
			VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: "csi.spiffe.io", ReadOnly: ptr.To(true)}},
		})
	}

	// The agent dials the broker UDS to request a mint — and nothing else. No key,
	// no SPIRE socket, no config.
	pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: dynamicBrokerVolumeName, MountPath: dynamicBrokerSocketDir})

	pod.Spec.InitContainers = append([]corev1.Container{dynamicSecretProxyContainer()}, pod.Spec.InitContainers...)
	return pod
}

func hasVolumeNamed(vols []corev1.Volume, name string) bool {
	for _, v := range vols {
		if v.Name == name {
			return true
		}
	}
	return false
}

func dynamicSecretProxyContainer() corev1.Container {
	return corev1.Container{
		Name:            dynamicSecretProxyName,
		Image:           SecretProxyImage(),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            []string{"--config=/etc/secret-proxy/config.yaml"},
		RestartPolicy:   ptr.To(corev1.ContainerRestartPolicyAlways), // native sidecar
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			RunAsNonRoot:             ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		Resources: corev1.ResourceRequirements{
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: dynamicBrokerVolumeName, MountPath: dynamicBrokerSocketDir},
			{Name: dynamicBrokerConfigVolume, MountPath: "/etc/secret-proxy", ReadOnly: true},
			{Name: dynamicBrokerKeyVolume, MountPath: dynamicBrokerKeyDir, ReadOnly: true},
			{Name: dynamicSpireVolumeName, MountPath: dynamicSpireDir, ReadOnly: true},
		},
	}
}
