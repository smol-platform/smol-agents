// Package builders — agent_serviceaccount.go
//
// AgentServiceAccount renders the ServiceAccount that AgentRun pods execute as.
// BuildAgentRunPod hardcodes the pod's serviceAccountName to "<agent>-agent";
// without this SA in the namespace, pod creation fails with "error looking up
// service account ... not found".
package builders

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	amv1 "github.com/smol-platform/smol-agents/operator/api/agentmodel/v1"
)

// AgentSAName returns the ServiceAccount name BuildAgentRunPod references.
// Single source of truth so the builder and the SA's creator agree.
func AgentSAName(agentName string) string { return agentName + "-agent" }

// AgentServiceAccount renders the SA the Agent's run pods execute as.
func AgentServiceAccount(agent *amv1.Agent) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      AgentSAName(agent.Name),
			Namespace: agent.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "smol-agents",
				"app.kubernetes.io/component": "agent",
				"agents.smol-agents.ai/agent": agent.Name,
			},
		},
	}
}
