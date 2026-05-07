package features

import (
	"strings"
	"testing"
)

func TestAllReturnsExpectedSet(t *testing.T) {
	got := All()
	if len(got) != 8 {
		t.Fatalf("All() returned %d, want 8", len(got))
	}
	want := []Feature{Identity, TransportPrivate, TransportPublic, Secrets, Sandbox, EBPF, Knative, Observability}
	for i, f := range want {
		if got[i] != f {
			t.Errorf("All()[%d] = %s, want %s", i, got[i], f)
		}
	}
}

func TestValid(t *testing.T) {
	for _, f := range All() {
		if !Valid(f) {
			t.Errorf("Valid(%s) = false", f)
		}
	}
	if Valid("garbage") {
		t.Error("Valid(garbage) = true")
	}
}

func TestConditionType(t *testing.T) {
	cases := map[Feature]string{
		Identity:         "IdentityReady",
		TransportPrivate: "TransportPrivateReady",
		TransportPublic:  "TransportPublicReady",
		Secrets:          "SecretsReady",
		Sandbox:          "SandboxReady",
		EBPF:             "EBPFReady",
		Knative:          "KnativeReady",
		Observability:    "ObservabilityReady",
	}
	for f, want := range cases {
		if got := ConditionType(f); got != want {
			t.Errorf("ConditionType(%s) = %q, want %q", f, got, want)
		}
	}
}

func TestConditionType_UnknownIsClearlyNamed(t *testing.T) {
	got := ConditionType("custom.thing")
	if !strings.Contains(got, "Unknown") {
		t.Errorf("expected Unknown prefix, got %q", got)
	}
}

func TestNoDuplicates(t *testing.T) {
	seen := make(map[Feature]struct{}, 8)
	for _, f := range All() {
		if _, dup := seen[f]; dup {
			t.Errorf("duplicate feature %s", f)
		}
		seen[f] = struct{}{}
	}
}
