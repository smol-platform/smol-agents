package v1

// Team labels mark an AgentRun/AgentSession as part of an AgentTeam. They live in
// the pure package (the lowest layer) so every consumer can reference one source
// of truth: the run-pod builder (inject team NATS context), the AgentTeam
// reconciler (map an owned run to its member), the A2A invoker (stamp team
// context onto delegated children), and the agentgateway. amv1 re-exports them.
const (
	// TeamLabel names the owning AgentTeam (set alongside the OwnerReference).
	TeamLabel = "runtime.agents.smol-agents.ai/team"
	// TeamMemberLabel marks a run/session as a named team member's worker.
	TeamMemberLabel = "runtime.agents.smol-agents.ai/team-member"
)
