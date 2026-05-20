package ebpfloader

import (
	"path/filepath"
	"testing"

	"github.com/stigen/smol-agents/pkg/ebpf"
)

func TestNewDefaultsPinRoot(t *testing.T) {
	l := New(Config{})
	if l.cfg.PinRoot != "/sys/fs/bpf/smol-agents" {
		t.Errorf("PinRoot = %q, want /sys/fs/bpf/smol-agents", l.cfg.PinRoot)
	}
}

func TestNewKeepsPinRoot(t *testing.T) {
	l := New(Config{PinRoot: "/tmp/bpf-test"})
	if l.cfg.PinRoot != "/tmp/bpf-test" {
		t.Errorf("PinRoot = %q", l.cfg.PinRoot)
	}
}

func TestPinPath(t *testing.T) {
	l := New(Config{PinRoot: "/sys/fs/bpf/x"})
	got := l.PinPath("syscalls")
	want := filepath.Join("/sys/fs/bpf/x", "syscalls")
	if got != want {
		t.Errorf("PinPath = %q, want %q", got, want)
	}
}

func TestProgramObjectPathRequired(t *testing.T) {
	// On non-Linux this exercises the noop loader; on Linux it exercises
	// the early validation in Run. Both should reject empty ObjectPath
	// without crashing.
	l := New(Config{Programs: []ebpf.Program{{Name: "syscalls"}}})
	_, err := l.Run(t.Context())
	if err == nil {
		t.Fatal("expected error for empty ObjectPath")
	}
}
