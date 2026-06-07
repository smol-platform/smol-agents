package team

import pure "github.com/smol-platform/smol-agents/pkg/agentmodel/v1"

// CoordinatorDecision builds the M5 Decision a coordinator issues on a member's
// gated (RequiresAction) run — the plan-approval path (multi-agent orchestration
// §4.5). It reuses M5 WHOLESALE: it copies the member's PendingAction.Token (M5
// ignores a mismatched/stale token) and records the coordinator as DecidedBy for
// audit. The live coordinator patches the result onto the member run's
// spec.decision (it owns the child via A2A ownership + RBAC); the existing M5
// pre-run gate then approves→Completed or denies→Cancelled. No new gate — just a
// non-human approver on the same fail-closed mechanism.
func CoordinatorDecision(pa pure.PendingAction, coordinator string, approve bool, reason string) pure.Decision {
	return pure.Decision{
		Token:     pa.Token,
		Approve:   approve,
		Reason:    reason,
		DecidedBy: "coordinator:" + coordinator,
	}
}
