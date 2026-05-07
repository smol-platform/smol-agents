package sandbox

import (
	"strings"
	"testing"
)

func TestDefaultSpec(t *testing.T) {
	s := DefaultSpec()
	if s.RuntimeClass != string(KindKataFC) {
		t.Errorf("default = %q, want %q", s.RuntimeClass, KindKataFC)
	}
	if s.AllowHostEscape {
		t.Error("default must not allow host escape")
	}
	if !s.IsHardened() {
		t.Error("default must be hardened")
	}
	if s.Kind() != KindKataFC {
		t.Errorf("Kind() = %s, want kata-fc", s.Kind())
	}
}

func TestKind_Valid(t *testing.T) {
	for _, k := range []Kind{KindKataFC, KindKataQEMU, KindKataCLH, KindGVisor, KindRunc} {
		if !k.Valid() {
			t.Errorf("%s should be valid", k)
		}
	}
	if Kind("garbage").Valid() {
		t.Error("garbage should be invalid")
	}
}

func TestKind_IsMicroVM(t *testing.T) {
	cases := map[Kind]bool{
		KindKataFC:   true,
		KindKataQEMU: true,
		KindKataCLH:  true,
		KindGVisor:   false,
		KindRunc:     false,
	}
	for k, want := range cases {
		if got := k.IsMicroVM(); got != want {
			t.Errorf("%s.IsMicroVM() = %v, want %v", k, got, want)
		}
	}
}

func TestKind_IsHardened(t *testing.T) {
	for _, k := range []Kind{KindKataFC, KindKataQEMU, KindKataCLH, KindGVisor} {
		if !k.IsHardened() {
			t.Errorf("%s should be hardened", k)
		}
	}
	if KindRunc.IsHardened() {
		t.Error("runc should not be hardened")
	}
}

func TestParseKind(t *testing.T) {
	cases := map[string]Kind{
		"kata-fc":   KindKataFC,
		"kata-qemu": KindKataQEMU,
		"kata-clh":  KindKataCLH,
		"gvisor":    KindGVisor,
		"runc":      KindRunc,
		"unknown":   KindRunc, // unknown defaults to runc so guard fires
		"":          KindRunc,
	}
	for s, want := range cases {
		if got := ParseKind(s); got != want {
			t.Errorf("ParseKind(%q) = %s, want %s", s, got, want)
		}
	}
}

func TestValidate_HappyPaths(t *testing.T) {
	for _, k := range []Kind{KindKataFC, KindKataQEMU, KindKataCLH, KindGVisor} {
		s := Spec{RuntimeClass: string(k)}
		if err := s.Validate(); err != nil {
			t.Errorf("%s rejected: %v", k, err)
		}
	}
}

func TestValidate_RuncRequiresAllowHostEscape(t *testing.T) {
	bad := Spec{RuntimeClass: "runc"}
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "AllowHostEscape") {
		t.Errorf("runc without override should fail (R-SBX-1): %v", err)
	}
	ok := Spec{RuntimeClass: "runc", AllowHostEscape: true}
	if err := ok.Validate(); err != nil {
		t.Errorf("runc with override should pass: %v", err)
	}
}

func TestValidate_EmptyAndUnknown(t *testing.T) {
	if err := (Spec{}).Validate(); err == nil {
		t.Error("empty RuntimeClass should fail")
	}
	if err := (Spec{RuntimeClass: "made-up"}).Validate(); err == nil {
		t.Error("unknown RuntimeClass should fail")
	}
}
