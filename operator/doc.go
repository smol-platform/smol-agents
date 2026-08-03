// Package operator hosts the Kubebuilder-built control plane for the
// smol-agents platform.
//
// The operator watches the agent-model CRD family (Agent, AgentRun,
// AgentSession, AgentTeam, AgentWorkflow, ModelProvider, ModelGateway,
// AgentPolicy, AgentNetwork, ...) plus the cluster-scoped AgentNodePool,
// which compiles to Karpenter NodePool + EC2NodeClass objects (or an
// externally-managed ClusterAutoscaler node-group ConfigMap) for kata
// microVM node provisioning.
package operator
