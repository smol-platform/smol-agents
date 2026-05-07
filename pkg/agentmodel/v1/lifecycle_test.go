package v1

import "testing"

func TestPhaseTerminal(t *testing.T) {
	cases := map[Phase]bool{
		PhasePending:        false,
		PhaseRunning:        false,
		PhaseRequiresAction: false,
		PhaseCompleted:      true,
		PhaseFailed:         true,
		PhaseCancelled:      true,
		PhaseExpired:        true,
	}
	for p, want := range cases {
		if got := p.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", p, got, want)
		}
	}
}

func TestCanTransition_AllowedEdges(t *testing.T) {
	for _, e := range allowedEdges {
		if err := CanTransition(e[0], e[1]); err != nil {
			t.Errorf("allowed edge %s → %s rejected: %v", e[0], e[1], err)
		}
	}
}

func TestCanTransition_TerminalIsAbsorbing(t *testing.T) {
	for _, term := range []Phase{PhaseCompleted, PhaseFailed, PhaseCancelled, PhaseExpired} {
		for _, dest := range []Phase{PhasePending, PhaseRunning, PhaseCompleted} {
			if dest == term {
				continue
			}
			if err := CanTransition(term, dest); err == nil {
				t.Errorf("expected rejection from terminal %s → %s", term, dest)
			}
		}
	}
}

func TestCanTransition_IllegalRejected(t *testing.T) {
	bad := [][2]Phase{
		{PhasePending, PhaseCompleted},        // skip Running
		{PhaseRunning, PhasePending},          // backwards
		{PhaseRequiresAction, PhaseCompleted}, // must go through Running
	}
	for _, b := range bad {
		if err := CanTransition(b[0], b[1]); err == nil {
			t.Errorf("expected rejection of %s → %s", b[0], b[1])
		}
	}
}

func TestCanTransition_InvalidPhase(t *testing.T) {
	if err := CanTransition(Phase("garbage"), PhaseRunning); err == nil {
		t.Error("invalid from-phase accepted")
	}
	if err := CanTransition(PhasePending, Phase("garbage")); err == nil {
		t.Error("invalid to-phase accepted")
	}
}

func TestCanTransition_Idempotent(t *testing.T) {
	// p → p is allowed (no-op reconciles).
	for _, p := range []Phase{PhasePending, PhaseRunning, PhaseCompleted} {
		if err := CanTransition(p, p); err != nil {
			t.Errorf("self-transition %s rejected: %v", p, err)
		}
	}
}
