package builders

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
	purev1 "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"
	"github.com/smol-platform/smol-agents/pkg/agentnet/plan"
)

func TestApplyRunSandbox(t *testing.T) {
	cases := []struct {
		class string
		want  string // "" means RuntimeClassName must be nil
	}{
		{"kata-fc", "kata-fc"},
		{"gvisor", "gvisor"},
		{"kata-qemu", "kata-qemu"},
		{"runc", ""}, // cluster-default runtime, not pinned
		{"", ""},
	}
	for _, tc := range cases {
		pod := &corev1.Pod{}
		ApplyRunSandbox(pod, tc.class)
		got := ""
		if pod.Spec.RuntimeClassName != nil {
			got = *pod.Spec.RuntimeClassName
		}
		if got != tc.want {
			t.Errorf("ApplyRunSandbox(%q): RuntimeClassName=%q, want %q", tc.class, got, tc.want)
		}
	}
}

// M1.16: an empty plan is the unchanged floor; a bound allow-list replaces the
// blanket public rule with per-allow rules (DNS + in-cluster preserved, metadata
// allow dropped).
func TestBuildEgressPolicyWithPlan(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"

	if base := BuildAgentRunEgressPolicy(run); len(base.Spec.Egress) != 3 {
		t.Fatalf("floor must have 3 rules, got %d", len(base.Spec.Egress))
	}
	if empt := BuildAgentRunEgressPolicyWithPlan(run, plan.NetworkPlan{}); len(empt.Spec.Egress) != 3 {
		t.Fatalf("empty plan must equal the 3-rule floor, got %d", len(empt.Spec.Egress))
	}

	p := plan.NetworkPlan{AllowRules: []purev1.EgressRule{
		{CIDR: "140.82.112.0/20", Ports: []int32{443}},
		{CIDR: "169.254.169.254/32"}, // metadata → must be dropped
	}}
	np := BuildAgentRunEgressPolicyWithPlan(run, p)
	if len(np.Spec.Egress) != 3 { // DNS + in-cluster + 1 surviving allow
		t.Fatalf("want dns+in-cluster+1 allow = 3 rules, got %d", len(np.Spec.Egress))
	}
	if np.Spec.Egress[0].Ports[0].Port.IntVal != 53 {
		t.Errorf("rule0 must still be DNS")
	}
	allow := np.Spec.Egress[2]
	if allow.To[0].IPBlock.CIDR != "140.82.112.0/20" || len(allow.Ports) != 1 || allow.Ports[0].Port.IntVal != 443 {
		t.Errorf("allow rule wrong: %+v ports=%+v", allow.To, allow.Ports)
	}
	for _, e := range np.Spec.Egress {
		for _, to := range e.To {
			if to.IPBlock != nil && to.IPBlock.CIDR == "0.0.0.0/0" {
				t.Errorf("blanket public rule must be replaced when an allow-list is bound")
			}
		}
	}
}

// AttachAgentNetwork is the Tier-2 datapath seam — a no-op in Phase 1 (Tier-1 is
// the egress NetworkPolicy). It must never mutate the pod yet, even for a plan
// that will eventually need a proxy sidecar, and must tolerate a nil pod.
func TestAttachAgentNetwork_NoOpPhase1(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}}}
	before := len(pod.Spec.Containers)

	AttachAgentNetwork(pod, plan.NetworkPlan{}) // empty plan
	AttachAgentNetwork(pod, plan.NetworkPlan{ProxyResources: []purev1.ResourceTarget{{Name: "a", Kind: "http", LocalPort: 8080, Gateway: "https://a"}}})
	if len(pod.Spec.Containers) != before || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.Volumes) != 0 {
		t.Errorf("Phase-1 AttachAgentNetwork must not mutate the pod: containers=%d init=%d vols=%d",
			len(pod.Spec.Containers), len(pod.Spec.InitContainers), len(pod.Spec.Volumes))
	}

	AttachAgentNetwork(nil, plan.NetworkPlan{ProxyResources: []purev1.ResourceTarget{{Name: "a"}}}) // must not panic
}

func TestBuildAgentRunEgressPolicy(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"

	np := BuildAgentRunEgressPolicy(run)

	if np.Name != "r1-egress" || np.Namespace != "tenant-a" {
		t.Fatalf("name/ns = %s/%s, want r1-egress/tenant-a", np.Name, np.Namespace)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Errorf("policyTypes = %v, want [Egress]", np.Spec.PolicyTypes)
	}
	if np.Spec.PodSelector.MatchLabels["agents.smol-agents.ai/run"] != "r1" {
		t.Errorf("podSelector must target the run pod, got %v", np.Spec.PodSelector.MatchLabels)
	}
	if len(np.Spec.Egress) != 3 {
		t.Fatalf("want 3 egress rules (dns, in-cluster, public), got %d", len(np.Spec.Egress))
	}

	// Rule 0: DNS on 53.
	if len(np.Spec.Egress[0].Ports) != 2 || np.Spec.Egress[0].Ports[0].Port.IntVal != 53 {
		t.Errorf("rule0 must be DNS/53, got %+v", np.Spec.Egress[0].Ports)
	}
	// Rule 1: in-cluster peers — an identity-based namespaceSelector (so the floor
	// allows pod-to-pod on Cilium, where bare CIDRs only match outside-cluster
	// entities) OR'd with the RFC1918 CIDRs — no port restriction (any port).
	rule1 := np.Spec.Egress[1]
	if len(rule1.To) != 4 || len(rule1.Ports) != 0 {
		t.Errorf("rule1 must be 1 namespaceSelector + 3 in-cluster CIDRs, all ports; got to=%d ports=%d",
			len(rule1.To), len(rule1.Ports))
	}
	if rule1.To[0].NamespaceSelector == nil {
		t.Error("rule1 must lead with an identity-based namespaceSelector so the egress floor permits " +
			"in-cluster pod-to-pod traffic on Cilium (CIDR-only peers are bypassed by cluster security identities)")
	}
	// Rule 2: public 0.0.0.0/0 on 443/80, with metadata + internal excepted.
	pub := np.Spec.Egress[2]
	if len(pub.To) != 1 || pub.To[0].IPBlock == nil || pub.To[0].IPBlock.CIDR != "0.0.0.0/0" {
		t.Fatalf("rule2 must be public 0.0.0.0/0, got %+v", pub.To)
	}
	except := pub.To[0].IPBlock.Except
	mustHave := map[string]bool{metadataBlockedCIDR: false, "10.0.0.0/8": false}
	for _, c := range except {
		if _, ok := mustHave[c]; ok {
			mustHave[c] = true
		}
	}
	for c, found := range mustHave {
		if !found {
			t.Errorf("public egress except-list missing %q (metadata/internal must be blocked): %v", c, except)
		}
	}
	if len(pub.Ports) != 2 {
		t.Errorf("public egress must be limited to 2 ports (443,80), got %d", len(pub.Ports))
	}
}

// knative-agents-8s1: the ingress floor is same-namespace-only — Ingress-only
// PolicyTypes, a pod selector matching the run/session labels, and a single
// From peer that is a NamespaceSelector on the object's own namespace (no
// ipBlock/0.0.0.0 peer, i.e. cross-namespace traffic is never allowed).
func TestBuildAgentRunIngressPolicy(t *testing.T) {
	run := &amv1.AgentRun{}
	run.Name = "r1"
	run.Namespace = "tenant-a"

	np := BuildAgentRunIngressPolicy(run)

	if np.Name != "r1-ingress" || np.Namespace != "tenant-a" {
		t.Fatalf("name/ns = %s/%s, want r1-ingress/tenant-a", np.Name, np.Namespace)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes = %v, want [Ingress] only", np.Spec.PolicyTypes)
	}
	if np.Spec.PodSelector.MatchLabels["agents.smol-agents.ai/run"] != "r1" {
		t.Errorf("podSelector must target the run pod, got %v", np.Spec.PodSelector.MatchLabels)
	}
	assertSameNamespaceOnlyIngress(t, np, "tenant-a")
}

func TestBuildAgentSessionIngressPolicy(t *testing.T) {
	sel := map[string]string{"agents.smol-agents.ai/run": "s1-session"}
	np := BuildAgentSessionIngressPolicy("s1-session", "tenant-b", sel)

	if np.Name != "s1-session-ingress" || np.Namespace != "tenant-b" {
		t.Fatalf("name/ns = %s/%s, want s1-session-ingress/tenant-b", np.Name, np.Namespace)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Errorf("policyTypes = %v, want [Ingress] only", np.Spec.PolicyTypes)
	}
	if np.Spec.PodSelector.MatchLabels["agents.smol-agents.ai/run"] != "s1-session" {
		t.Errorf("podSelector must target the session worker pod, got %v", np.Spec.PodSelector.MatchLabels)
	}
	assertSameNamespaceOnlyIngress(t, np, "tenant-b")
}

// assertSameNamespaceOnlyIngress asserts the rendered policy has exactly one
// ingress rule with exactly one From peer: a NamespaceSelector matching ns via
// kubernetes.io/metadata.name, and nothing else (no ipBlock/CIDR peer, no
// ports restriction beyond that single peer).
func assertSameNamespaceOnlyIngress(t *testing.T, np *networkingv1.NetworkPolicy, ns string) {
	t.Helper()
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("want exactly 1 ingress rule, got %d", len(np.Spec.Ingress))
	}
	rule := np.Spec.Ingress[0]
	if len(rule.From) != 1 {
		t.Fatalf("want exactly 1 From peer (same-namespace only), got %d: %+v", len(rule.From), rule.From)
	}
	peer := rule.From[0]
	if peer.IPBlock != nil {
		t.Errorf("From peer must not be an ipBlock (would allow cross-namespace/external traffic): %+v", peer.IPBlock)
	}
	if peer.NamespaceSelector == nil {
		t.Fatalf("From peer must be a NamespaceSelector, got %+v", peer)
	}
	if got := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != ns {
		t.Errorf("NamespaceSelector must match the object's own namespace %q, got %q", ns, got)
	}
}
