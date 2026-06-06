package builders

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1 "github.com/smol-platform/smol-agents/operator/api/v1"
)

// BuildAgentTerminalService renders the ClusterIP Service fronting an agent's
// ttyd terminal ports (M4.9 — the first serving-path Service builder). The
// cmd/agentterminal gateway dials this Service (driver/viewer port) after it has
// authenticated the human + resolved an AttachGrant. ClusterIP (not exposed
// outside the cluster); the gateway is the only authorized client, enforced by
// BuildAgentTerminalIngress + the ttyd auth header.
func BuildAgentTerminalService(cr *v1.SmolAgent) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-terminal",
			Namespace: cr.Namespace,
			Labels:    Labels(cr),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: Selector(cr),
			Ports: []corev1.ServicePort{
				{Name: "ttyd-driver", Port: TerminalDriverPort, TargetPort: intstr.FromInt(TerminalDriverPort), Protocol: corev1.ProtocolTCP},
				{Name: "ttyd-viewer", Port: TerminalViewerPort, TargetPort: intstr.FromInt(TerminalViewerPort), Protocol: corev1.ProtocolTCP},
			},
		},
	}
}

// BuildAgentTerminalIngress renders the ingress NetworkPolicy that admits the
// ttyd ports ONLY from the cmd/agentterminal gateway's pods (M4.9). It selects
// the agent's serving pods and, as the sole ingress rule, allows the
// driver/viewer ports from peers matching the gateway's namespace + pod labels —
// so every other source (including same-namespace pods) is denied for those
// ports. Defense-in-depth with the ttyd auth header: even a permitted peer must
// present the gateway-injected X-Smol-Attach header.
func BuildAgentTerminalIngress(cr *v1.SmolAgent, gatewayNamespace string, gatewaySelector map[string]string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	driver := intstr.FromInt(TerminalDriverPort)
	viewer := intstr.FromInt(TerminalViewerPort)

	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-terminal-ingress",
			Namespace: cr.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "smol-agents",
				"app.kubernetes.io/component": "terminal",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: Selector(cr)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": gatewayNamespace},
					},
					PodSelector: &metav1.LabelSelector{MatchLabels: gatewaySelector},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &tcp, Port: &driver},
					{Protocol: &tcp, Port: &viewer},
				},
			}},
		},
	}
}
