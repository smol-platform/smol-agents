// Package builders — run_sandbox.go
//
// Containment for the AgentRun and AgentSession datapaths (the pods that
// execute untrusted harnesses/CLIs). Controls the run/session pod previously
// lacked:
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
//   - BuildIngressPolicy (knative-agents-8s1) renders a same-namespace-only
//     ingress floor: these pods expose no ports today, but with no ingress
//     policy at all they are dialable from ANY pod in ANY namespace — a
//     tenant-boundary hole under D1.
//
// Together these make the documented "microVM + network cage" containment
// actually hold on the run/session path — the only agent datapath left since
// the legacy SmolAgent serving platform was removed.
package builders

import (
	"fmt"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	purev1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentnet/plan"
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

// APIServerEgressRule allows egress to the kube-apiserver endpoints on apiPort
// (M1.18). The default floor permits public IPs only on 80/443, so an apiserver
// reached via a node's PUBLIC IP:6443 (single-node / public-IP clusters, e.g.
// k0s on a cloud VM) is blocked — breaking any run that must call the apiserver
// (notably A2A, which creates child AgentRuns). The reconciler discovers the
// endpoint IPs from the kubernetes EndpointSlice; this renders the allow rule.
// Returns nil for no valid IPs, leaving the floor unchanged.
func APIServerEgressRule(ips []string, apiPort int32) *networkingv1.NetworkPolicyEgressRule {
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(ips))
	for _, ip := range ips {
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			continue
		}
		bits := 32
		if addr.Is6() {
			bits = 128
		}
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: fmt.Sprintf("%s/%d", ip, bits)},
		})
	}
	if len(peers) == 0 {
		return nil
	}
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(apiPort)
	return &networkingv1.NetworkPolicyEgressRule{
		To:    peers,
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
	}
}

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
	return BuildAgentRunEgressPolicyWithPlan(run, plan.NetworkPlan{})
}

// BuildAgentRunEgressPolicyWithPlan layers a bound NetworkPlan's allow-list onto
// the run's egress cage (an empty plan is the unchanged default-deny floor).
func BuildAgentRunEgressPolicyWithPlan(run *amv1.AgentRun, p plan.NetworkPlan) *networkingv1.NetworkPolicy {
	return BuildEgressPolicyWithPlan(run.Name+"-egress", run.Namespace, "agent-run",
		map[string]string{"agents.smol-agents.ai/run": run.Name}, p)
}

// BuildAgentSessionEgressPolicy is the same default-deny egress cage for a
// long-running AgentSession worker pod.
func BuildAgentSessionEgressPolicy(name, namespace string, podSelector map[string]string) *networkingv1.NetworkPolicy {
	return BuildAgentSessionEgressPolicyWithPlan(name, namespace, podSelector, plan.NetworkPlan{})
}

// BuildAgentSessionEgressPolicyWithPlan layers a bound NetworkPlan onto a session
// worker's egress cage.
func BuildAgentSessionEgressPolicyWithPlan(name, namespace string, podSelector map[string]string, p plan.NetworkPlan) *networkingv1.NetworkPolicy {
	return BuildEgressPolicyWithPlan(name+"-egress", namespace, "agent-session", podSelector, p)
}

// BuildAgentRunIngressPolicy renders the same-namespace-only ingress floor for
// a run pod.
func BuildAgentRunIngressPolicy(run *amv1.AgentRun) *networkingv1.NetworkPolicy {
	return BuildIngressPolicy(run.Name+"-ingress", run.Namespace, "agent-run",
		map[string]string{"agents.smol-agents.ai/run": run.Name})
}

// BuildAgentSessionIngressPolicy is the same same-namespace-only ingress floor
// for a long-running AgentSession worker pod.
func BuildAgentSessionIngressPolicy(name, namespace string, podSelector map[string]string) *networkingv1.NetworkPolicy {
	return BuildIngressPolicy(name+"-ingress", namespace, "agent-session", podSelector)
}

// BuildIngressPolicy renders the same-namespace-only ingress floor: run and
// session worker pods declare no ContainerPorts and are never dialed on the
// current datapath (runs are a fire-and-forget executor; session workers pull
// turns by subscribing to NATS), but with NO ingress NetworkPolicy at all they
// are reachable from ANY pod in ANY namespace — a tenant-boundary hole under
// D1 (multi-tenant/untrusted). An empty Ingress rule list would be a total
// default-deny and is tempting since these pods expose nothing today, but
// same-namespace is the conservative floor: it closes the cross-namespace hole
// without pre-breaking in-namespace sidecars/tooling that might one day probe
// or attach to the pod. A stricter full-deny is available later precisely
// because these pods declare no ports (knative-agents-8s1).
func BuildIngressPolicy(name, namespace, component string, podSelector map[string]string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "smol-agents",
				"app.kubernetes.io/component": component,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podSelector},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
				}},
			}},
		},
	}
}

// AttachAgentNetwork is the Tier-2 datapath seam for a bound NetworkPlan: where
// a TraT-injecting proxy sidecar (p.ProxyNeeded) and the eBPF egress-redirect
// (p.EbpfNeeded) attach to the run/session pod. Tier-1 enforcement (the
// default-deny + allow-list NetworkPolicy from BuildEgressPolicyWithPlan) is what
// ships and is wired in both reconcilers; the in-pod redirect lands with the
// secret-proxy + SPIRE injection work, so this is intentionally a no-op in
// Phase 1. Kept as a named, called seam so the integration point is explicit
// (and so an empty/Tier-1-only plan never silently expects a sidecar). M1.16.
//
// Because this is a no-op, callers MUST first gate on checkTier2Wired: a plan that
// needs the proxy sidecar (ProxyNeeded) or an eBPF tier (EbpfNeeded) is held
// fail-closed, so a run/session is never scheduled believing this unwired,
// e2e-only datapath is enforcing its egress (c5r.20).
func AttachAgentNetwork(pod *corev1.Pod, p plan.NetworkPlan) {
	if pod == nil || (!p.ProxyNeeded() && !p.EbpfNeeded()) {
		return
	}
	// Phase 2: inject the proxy sidecar + eBPF redirect from the plan's proxy
	// resources / redirect rules here. No-op until that datapath lands.
}

// BuildEgressPolicyWithPlan renders the egress cage, layering a NetworkPlan's
// allow-list on top of the default-deny floor. With an empty plan it is
// byte-identical to the default-deny floor (DNS + in-cluster + public 80/443).
// With allow rules it REPLACES the blanket public 80/443 rule with
// per-(CIDR,ports,proto) rules, so a bound AgentNetwork *tightens* (never
// widens) what the pod can reach — DNS, in-cluster, and the metadata block are
// always preserved. An allow CIDR overlapping the metadata range is dropped
// (defense; the plan compositor already rejects them).
func BuildEgressPolicyWithPlan(name, namespace, component string, podSelector map[string]string, p plan.NetworkPlan) *networkingv1.NetworkPolicy {
	np := buildEgressPolicy(name, namespace, component, podSelector)
	if len(p.AllowRules) == 0 {
		return np // unchanged floor
	}
	// Keep DNS (rule 0) + in-cluster (rule 1); replace the public rule (rule 2)
	// with the explicit per-allow rules.
	egress := np.Spec.Egress[:2]
	for _, r := range p.AllowRules {
		if overlapsMetadata(r.CIDR) {
			continue
		}
		egress = append(egress, egressRuleFromAllow(r))
	}
	np.Spec.Egress = egress
	return np
}

// egressRuleFromAllow translates one allow-list entry into a NetworkPolicy
// egress rule. Empty Ports = any port; an empty protocol defaults to TCP when
// ports are listed.
func egressRuleFromAllow(r purev1.EgressRule) networkingv1.NetworkPolicyEgressRule {
	rule := networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: r.CIDR}}},
	}
	proto := corev1.ProtocolTCP
	if r.Protocol == "udp" {
		proto = corev1.ProtocolUDP
	}
	for _, port := range r.Ports {
		pp := intstr.FromInt32(port)
		rule.Ports = append(rule.Ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &pp})
	}
	return rule
}

// overlapsMetadata reports whether cidr intersects the 169.254/16 link-local
// (instance-metadata) range — such a rule is never honored on the egress floor.
func overlapsMetadata(cidr string) bool {
	ll := netip.MustParsePrefix(metadataBlockedCIDR)
	pfx, err := netip.ParsePrefix(cidr)
	if err != nil {
		if addr, aerr := netip.ParseAddr(cidr); aerr == nil {
			pfx = netip.PrefixFrom(addr, addr.BitLen())
		} else {
			return false // unparseable — let validation handle it
		}
	}
	return ll.Overlaps(pfx)
}

// buildEgressPolicy renders the shared default-deny-egress NetworkPolicy: DNS +
// in-cluster (any port) + public 80/443, with metadata/link-local blocked.
func buildEgressPolicy(name, namespace, component string, podSelector map[string]string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	p53 := intstr.FromInt32(53)
	p80 := intstr.FromInt32(80)
	p443 := intstr.FromInt32(443)

	// In-cluster allow peers. The empty namespaceSelector is the IDENTITY-based
	// peer (it selects every pod in every namespace) that makes this floor work on
	// identity-aware CNIs like Cilium, where an ipBlock CIDR only ever matches
	// entities OUTSIDE the cluster — in-cluster pods carry a security identity, so
	// a bare CIDR rule never authorizes pod-to-pod traffic (the worker→gateway,
	// →NATS, →AgentFS-S3 path would be silently denied). The RFC1918 ipBlock peers
	// are kept alongside it for CIDR-based CNIs and for host-network / node-IP
	// targets that have no pod identity. Peers in a To-list are OR'd, so this only
	// ever widens reachability to in-cluster destinations — the floor's intent.
	internalPeers := make([]networkingv1.NetworkPolicyPeer, 0, len(clusterInternalCIDRs)+1)
	internalPeers = append(internalPeers, networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{},
	})
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
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "smol-agents",
				"app.kubernetes.io/component": component,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podSelector},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				// DNS resolution (cluster + upstream).
				{Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &p53},
					{Protocol: &tcp, Port: &p53},
				}},
				// In-cluster services (gateway, AgentFS S3, NATS, …) on any port —
				// an identity-based namespaceSelector (Cilium-safe) OR'd with the
				// RFC1918 CIDRs (see internalPeers above).
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
