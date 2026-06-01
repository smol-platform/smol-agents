// Package builders — run_sandbox.go
//
// Containment for the AgentRun datapath (the pod that executes untrusted
// harnesses/CLIs). Two controls the run pod previously lacked:
//
//   - ApplyRunSandbox pins the resolved sandbox RuntimeClass (default kata-fc)
//     so a run executes inside a real isolation boundary instead of the
//     cluster-default runtime (runc / shared host kernel). The controller
//     resolves the class fail-closed (see resolveRunSandbox); this only stamps
//     the pod.
//   - BuildAgentRunEgressPolicy renders a default-deny-egress NetworkPolicy so a
//     compromised harness cannot reach the cloud instance-metadata endpoint
//     (the canonical SSRF / credential-theft target) or open arbitrary outbound
//     channels. It allows DNS, in-cluster traffic, and public HTTP(S) only.
//
// Together these make the documented "microVM + egress cage" containment
// actually hold on the run path, not just the long-lived serving path.
package builders

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	pkgsandbox "github.com/smol-platform/smol-agents/pkg/sandbox"
)

// clusterInternalCIDRs are treated as in-cluster (pod/service networks live in
// RFC1918): a run may reach in-cluster services (model gateway, AgentFS S3,
// kube-dns) on any port. They are carved out of the public egress rule so that
// rule only ever matches the public internet.
var clusterInternalCIDRs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

// metadataBlockedCIDR is the link-local range (incl. 169.254.169.254, the
// AWS/GCP/Azure instance-metadata endpoint): never reachable from a run pod.
const metadataBlockedCIDR = "169.254.0.0/16"

// ApplyRunSandbox pins the run pod's RuntimeClass to a resolved, hardened class.
// An empty class or "runc" leaves RuntimeClassName nil (cluster-default runtime)
// — the controller's resolveRunSandbox is responsible for the R-SBX-1 guard, so
// reaching here with runc means an operator deliberately allowed host runtime.
func ApplyRunSandbox(pod *corev1.Pod, class string) {
	if pod == nil || class == "" {
		return
	}
	if pkgsandbox.ParseKind(class) == pkgsandbox.KindRunc {
		return // cluster-default runtime; nothing to pin
	}
	pod.Spec.RuntimeClassName = ptr.To(class)
}

// BuildAgentRunEgressPolicy renders the default-deny-egress NetworkPolicy that
// cages a run pod: DNS is allowed, in-cluster destinations are allowed on any
// port, and the public internet is reachable only on 80/443 — with the
// link-local / instance-metadata range blocked everywhere. A tighter per-Agent
// allow-list (AgentNetwork CIDRs) can layer on top later.
func BuildAgentRunEgressPolicy(run *amv1.AgentRun) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	p53 := intstr.FromInt32(53)
	p80 := intstr.FromInt32(80)
	p443 := intstr.FromInt32(443)

	internalPeers := make([]networkingv1.NetworkPolicyPeer, 0, len(clusterInternalCIDRs))
	for _, c := range clusterInternalCIDRs {
		internalPeers = append(internalPeers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: c},
		})
	}
	// Public = everything except in-cluster (handled above) and metadata/link-local.
	publicExcept := append([]string{metadataBlockedCIDR}, clusterInternalCIDRs...)

	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.Name + "-egress",
			Namespace: run.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "smol-agents",
				"app.kubernetes.io/component": "agent-run",
				"agents.smol-agents.ai/run":   run.Name,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"agents.smol-agents.ai/run": run.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				// DNS resolution (cluster + upstream).
				{Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &p53},
					{Protocol: &tcp, Port: &p53},
				}},
				// In-cluster services (gateway, AgentFS S3, …) on any port.
				{To: internalPeers},
				// Public internet on HTTP(S) only; metadata/link-local blocked.
				{
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: publicExcept},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &p443},
						{Protocol: &tcp, Port: &p80},
					},
				},
			},
		},
	}
}
