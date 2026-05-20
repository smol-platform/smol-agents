package builders

import "testing"

func TestBuildRuntimeClassKataFC_HasOverhead(t *testing.T) {
	rc := BuildRuntimeClassKataFC()
	if rc.Overhead == nil {
		t.Fatal("kata-fc RuntimeClass must declare Overhead so Karpenter sizes nodes for the microVM")
	}
	if rc.Overhead.PodFixed.Cpu().IsZero() || rc.Overhead.PodFixed.Memory().IsZero() {
		t.Errorf("overhead must set cpu+memory, got %v", rc.Overhead.PodFixed)
	}
}

func TestBuildRuntimeClassGVisor_Handler(t *testing.T) {
	if h := BuildRuntimeClassGVisor().Handler; h != "runsc" {
		t.Errorf("gvisor handler = %q, want runsc", h)
	}
}
