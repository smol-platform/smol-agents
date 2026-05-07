package v1

import "fmt"

// Phase is the AgentRun lifecycle state. Names mirror the OpenAI
// Assistants API to keep the vocabulary familiar.
//
// Implements R-AM-LIF-1.
type Phase string

const (
	PhasePending        Phase = "Pending"
	PhaseRunning        Phase = "Running"
	PhaseRequiresAction Phase = "RequiresAction"
	PhaseCompleted      Phase = "Completed"
	PhaseFailed         Phase = "Failed"
	PhaseCancelled      Phase = "Cancelled"
	PhaseExpired        Phase = "Expired"
)

// Terminal returns true if p is one of the terminal states.
func (p Phase) Terminal() bool {
	switch p {
	case PhaseCompleted, PhaseFailed, PhaseCancelled, PhaseExpired:
		return true
	}
	return false
}

// Valid returns true if p is a known phase.
func (p Phase) Valid() bool {
	switch p {
	case PhasePending, PhaseRunning, PhaseRequiresAction,
		PhaseCompleted, PhaseFailed, PhaseCancelled, PhaseExpired:
		return true
	}
	return false
}

// CanTransition returns nil iff the runtime is allowed to move from
// `from` to `to`. Implements the state-machine edges described in
// R-AM-LIF-1.
func CanTransition(from, to Phase) error {
	if !from.Valid() {
		return fmt.Errorf("agentmodel: invalid from-phase %q", from)
	}
	if !to.Valid() {
		return fmt.Errorf("agentmodel: invalid to-phase %q", to)
	}
	if from == to {
		return nil
	}
	if from.Terminal() {
		return fmt.Errorf("agentmodel: %q is terminal; cannot transition to %q", from, to)
	}
	for _, edge := range allowedEdges {
		if edge[0] == from && edge[1] == to {
			return nil
		}
	}
	return fmt.Errorf("agentmodel: illegal transition %q → %q", from, to)
}

// Allowed transitions. Mirrored exactly in spec/quint/agent_execution.qnt
// so the formal model and the code share a single shape.
var allowedEdges = [][2]Phase{
	{PhasePending, PhaseRunning},
	{PhasePending, PhaseCancelled},
	{PhasePending, PhaseExpired},
	{PhasePending, PhaseFailed},

	{PhaseRunning, PhaseRequiresAction},
	{PhaseRunning, PhaseCompleted},
	{PhaseRunning, PhaseFailed},
	{PhaseRunning, PhaseCancelled},
	{PhaseRunning, PhaseExpired},

	{PhaseRequiresAction, PhaseRunning},
	{PhaseRequiresAction, PhaseCancelled},
	{PhaseRequiresAction, PhaseExpired},
	{PhaseRequiresAction, PhaseFailed},
}
