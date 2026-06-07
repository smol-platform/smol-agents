package team

// Team hooks (multi-agent orchestration §4.5) — the team-lifecycle analog of
// Claude Code's TeammateIdle / TaskCreated / TaskCompleted hooks, expressed as
// the platform's fail-closed, operator-enforced gate model: a team attaches hooks
// that can veto a task creation/completion or re-queue an idle member. This is
// the pure evaluation the coordinator (or an admission gate) consults; attaching
// hooks to a team CRD + the operator firing them is the deployment-side wiring.

// TeamHookEvent is a gated team-lifecycle event.
type TeamHookEvent string

const (
	HookTeammateIdle  TeamHookEvent = "TeammateIdle"
	HookTaskCreated   TeamHookEvent = "TaskCreated"
	HookTaskCompleted TeamHookEvent = "TaskCompleted"
)

// HookAction is a hook's verdict on an event.
type HookAction string

const (
	HookAllow   HookAction = "allow"
	HookVeto    HookAction = "veto"
	HookRequeue HookAction = "requeue"
)

// TeamHook gates one event with an action.
type TeamHook struct {
	Event  TeamHookEvent
	Action HookAction
	Reason string
}

// EvaluateHooks returns the first non-allow action declared for event (veto /
// requeue), else allow. Fail-closed by construction: a hook only ever tightens
// (an absent hook = allow; a present veto/requeue wins). Deterministic in hook
// order so a team's gate behavior is predictable.
func EvaluateHooks(event TeamHookEvent, hooks []TeamHook) (HookAction, string) {
	for _, h := range hooks {
		if h.Event != event {
			continue
		}
		if h.Action != "" && h.Action != HookAllow {
			return h.Action, h.Reason
		}
	}
	return HookAllow, ""
}
