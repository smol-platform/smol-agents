// Package builders — agent_a2a_rbac.go
//
// A2A (agent-to-agent) RBAC: the namespaced Role + RoleBinding that grant an
// A2A-capable Agent's run pods authority to create and observe CHILD AgentRuns
// in their OWN namespace only (never cluster-wide, never cross-tenant). The
// reconciler creates these only for an Agent that declares a kind=agent tool, so
// a non-A2A Agent's pods keep zero apiserver authority (M3.6 / D1).
package builders

import (
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

const a2aAPIGroup = "runtime.agents.smol-agents.ai"

// a2aDepthLabel mirrors pkg/agentruntime/invokers.DepthLabel: the depth a child
// AgentRun sits at in the A2A delegation tree. The parent's invoker stamps it on
// children; BuildAgentRunPod reads it back into the child pod's A2A_DEPTH env so
// the child's own invoker enforces the recursion bound. Top-level runs lack it
// (depth 0). Kept as a literal here to avoid the operator module depending on
// the root module's invokers package.
const a2aDepthLabel = "agents.smol-agents.ai/a2a-depth"

// AgentA2ARoleName is the Role/RoleBinding name for an Agent's A2A grant.
func AgentA2ARoleName(agentName string) string { return agentName + "-a2a" }

// AgentA2ARole permits creating + observing AgentRuns (and reading their status)
// in the Agent's own namespace. It deliberately omits "delete" — children are
// reclaimed via OwnerReference GC — and is namespace-scoped (a Role, not a
// ClusterRole), so an exploited child-spawn can never escape the tenant.
func AgentA2ARole(agent *amv1.Agent) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: a2aMeta(agent),
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{a2aAPIGroup},
				Resources: []string{"agentruns"},
				Verbs:     []string{"create", "get", "list", "watch"},
			},
			{
				APIGroups: []string{a2aAPIGroup},
				Resources: []string{"agentruns/status"},
				Verbs:     []string{"get"},
			},
		},
	}
}

// AgentA2ARoleBinding binds the A2A Role to the Agent's run ServiceAccount.
func AgentA2ARoleBinding(agent *amv1.Agent) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: a2aMeta(agent),
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     AgentA2ARoleName(agent.Name),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      AgentSAName(agent.Name),
			Namespace: agent.Namespace,
		}},
	}
}

func a2aMeta(agent *amv1.Agent) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      AgentA2ARoleName(agent.Name),
		Namespace: agent.Namespace,
		Labels: map[string]string{
			"app.kubernetes.io/name":      "smol-agents",
			"app.kubernetes.io/component": "agent-a2a",
			"agents.smol-agents.ai/agent": agent.Name,
		},
	}
}
