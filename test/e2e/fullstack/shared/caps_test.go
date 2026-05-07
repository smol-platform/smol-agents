package shared

import "testing"

func TestCaps_Has(t *testing.T) {
	all := CapKubernetes | CapEBPF | CapKata
	if !all.Has(CapEBPF) {
		t.Error("expected superset to satisfy subset")
	}
	if all.Has(CapWebhook) {
		t.Error("missing cap should not be satisfied")
	}
}

func TestCaps_String_Stable(t *testing.T) {
	c := CapKubernetes | CapEBPF
	got := c.String()
	if got != "ebpf,kubernetes" {
		t.Errorf("String() = %q, want stable sorted form", got)
	}
}

func TestCaps_MustParse_RoundTrip(t *testing.T) {
	want := CapEBPF | CapKata | CapSPIRE
	round := MustParse(want.String())
	if round != want {
		t.Errorf("round trip lost bits: got %s, want %s", round, want)
	}
}

func TestCaps_MustParse_None(t *testing.T) {
	if MustParse("none") != 0 {
		t.Error("'none' should be zero caps")
	}
	if MustParse("") != 0 {
		t.Error("empty should be zero caps")
	}
}

func TestCaps_MustParse_PanicsOnUnknown(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on unknown cap")
		}
	}()
	MustParse("kubernetes,not-a-real-cap")
}
