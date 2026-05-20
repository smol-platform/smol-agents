package builders

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/stigen/smol-agents/operator/api/v1"
)

// BuildAgentConfigMap renders the ConfigMap that the agent runtime mounts
// to populate pkg/config.Agent. The shape mirrors deploy/helm/templates/configmap.yaml.
func BuildAgentConfigMap(cr *v1.SmolAgent) *corev1.ConfigMap {
	mode := cr.Spec.Mode
	if mode == "" {
		mode = nonEmpty(cr.Spec.Features.Identity.Mode, "strict")
	}
	cm := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-config",
			Namespace: cr.Namespace,
			Labels:    Labels(cr),
		},
		Data: map[string]string{
			"agent.yaml": renderAgentYAML(cr, mode),
		},
	}
	return cm
}

func renderAgentYAML(cr *v1.SmolAgent, mode string) string {
	out := ""
	add := func(format string, args ...any) { out += fmt.Sprintf(format, args...) }

	add("mode: %s\n", mode)
	add("trustDomain: %s\n", cr.Spec.TrustDomain)

	add("identity:\n")
	add("  workloadAPI: %s\n", nonEmpty(cr.Spec.Features.Identity.WorkloadAPI, "unix:///run/spire/agent-sockets/api.sock"))
	add("  bootTimeout: %ds\n", boundedSeconds(cr.Spec.Features.Identity.BootTimeoutSeconds, 30))

	add("transport:\n")
	if cr.Spec.Features.Transport.Private.Enabled {
		add("  private:\n")
		add("    addr: %q\n", nonEmpty(cr.Spec.Features.Transport.Private.Addr, "0.0.0.0:8443"))
		add("    authorize:\n")
		auths := cr.Spec.Features.Transport.Private.Authorize
		if len(auths) == 0 {
			auths = []string{"any:spiffe://" + cr.Spec.TrustDomain}
		}
		for _, a := range auths {
			add("      - %q\n", a)
		}
	}
	if cr.Spec.Features.Transport.Public.Enabled {
		add("  public:\n")
		add("    addr: %q\n", nonEmpty(cr.Spec.Features.Transport.Public.Addr, "0.0.0.0:8444"))
		add("    certPath: %s\n", cr.Spec.Features.Transport.Public.CertPath)
		add("    keyPath: %s\n", cr.Spec.Features.Transport.Public.KeyPath)
	}

	if cr.Spec.Features.Secrets.Enabled {
		add("secrets:\n")
		add("  brokerSocket: %s\n", nonEmpty(cr.Spec.Features.Secrets.BrokerSocket, "/run/secret-broker/secret-broker.sock"))
		add("  maxLeaseTTL: %ds\n", boundedSeconds(cr.Spec.Features.Secrets.MaxLeaseTTLSeconds, 900))
	}

	if cr.Spec.Features.EBPF.Enabled {
		add("ebpf:\n")
		add("  programs:\n")
		progs := cr.Spec.Features.EBPF.Programs
		if len(progs) == 0 {
			progs = []string{"syscalls", "network"}
		}
		for _, p := range progs {
			add("    - %s\n", p)
		}
		add("  objectsDir: /usr/share/smol-agents/bpf\n")
	}

	add("sandbox:\n")
	add("  runtimeClass: %s\n", nonEmpty(cr.Spec.Features.Sandbox.RuntimeClass, "kata-fc"))

	if cr.Spec.Features.Observability.Enabled {
		add("observability:\n")
		add("  serviceName: %s\n", nonEmpty(cr.Spec.Features.Observability.ServiceName, "smol-agent"))
		if cr.Spec.Features.Observability.OTLPEndpoint != "" {
			add("  otlpEndpoint: %s\n", cr.Spec.Features.Observability.OTLPEndpoint)
		}
	}

	add("runtime:\n")
	add("  drainTimeout: 30s\n")
	add("  shutdownTimeout: 5s\n")
	add("  healthAddr: \"0.0.0.0:8080\"\n")

	return out
}

func nonEmpty(s, dflt string) string {
	if s == "" {
		return dflt
	}
	return s
}

func boundedSeconds(v int32, dflt int32) int32 {
	if v <= 0 {
		return dflt
	}
	return v
}
